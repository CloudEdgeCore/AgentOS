//go:build integration

// Package control_test verifies the fenced Runtime Protocol boundary against a
// real PostgreSQL and, when the Wasmtime provider is built, the v0.1
// acceptance baseline item "the same AgentVersion runs on at least two
// different Runtime Providers".
package control_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	runtimev1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/runtime/v1alpha1"
	"github.com/bian-cloud-skill/agentos/internal/kernel/admission"
	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	"github.com/bian-cloud-skill/agentos/internal/kernel/scheduler"
	kernelstore "github.com/bian-cloud-skill/agentos/internal/kernel/store"
	postgresstore "github.com/bian-cloud-skill/agentos/internal/kernel/store/postgres"
	"github.com/bian-cloud-skill/agentos/internal/platform/artifact"
	"github.com/bian-cloud-skill/agentos/internal/platform/migrate"
	runtimecontrol "github.com/bian-cloud-skill/agentos/internal/runtime/control"
	"github.com/bian-cloud-skill/agentos/internal/runtime/reference"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	conformanceTenant   = "tenant-a"
	conformanceVersion  = "conformance@1"
	referenceProvider   = "reference-go"
	referenceABI        = "agentos.reference/v1"
	referenceSchema     = "agentos.reference-state/v1"
	wasmtimeProvider    = "wasmtime"
	wasmtimeABI         = "agentos.wasm-component/v1"
	wasmtimeSchema      = "agentos.wasm-logical-state/v1"
	terminalWaitTimeout = 60 * time.Second
)

// scenario describes one provider leg of the conformance run.
type scenario struct {
	key          string
	runtimeClass string
	instanceID   string
	spec         string
	provider     string
	runtimeABI   string
	checkpoint   string
}

type conformanceEnv struct {
	pool       *pgxpool.Pool
	store      *postgresstore.Store
	grpcAddr   string
	tenant     string
	versionRef string
	versionID  uuid.UUID
	taskID     uuid.UUID
}

func TestSameAgentVersionRunsOnBothProviders(t *testing.T) {
	env := newConformanceEnv(t)

	t.Run("reference-go", func(t *testing.T) {
		scenario := scenario{
			key: "conformance-reference", runtimeClass: "oci", instanceID: "worker-reference-1",
			spec:     `{"priority":70,"deadline":"2099-08-14T12:00:00Z","budget":{"tokens":500,"costUsd":2,"toolCalls":10,"wallSeconds":60},"placement":{"runtimeClasses":["oci"],"preferredClass":"oci","region":"cn-east","cpuMillis":100,"memoryMiB":128,"llmConcurrency":1}}`,
			provider: referenceProvider, runtimeABI: referenceABI, checkpoint: referenceSchema,
		}
		env.prepareScenario(t, scenario)
		env.driveReferenceWorker(t)
		env.assertConformance(t, scenario)
	})

	t.Run("wasmtime", func(t *testing.T) {
		scenario := scenario{
			key: "conformance-wasmtime", runtimeClass: "wasmtime", instanceID: "worker-wasmtime-1",
			spec:     `{"priority":70,"deadline":"2099-08-14T12:00:00Z","budget":{"tokens":500,"costUsd":2,"toolCalls":10,"wallSeconds":60},"placement":{"runtimeClasses":["wasmtime"],"preferredClass":"wasmtime","region":"cn-east","cpuMillis":100,"memoryMiB":128,"llmConcurrency":1},"runtime":{"componentPath":"agent.wasm"}}`,
			provider: wasmtimeProvider, runtimeABI: wasmtimeABI, checkpoint: wasmtimeSchema,
		}
		env.prepareScenario(t, scenario)
		env.driveWasmtimeWorker(t)
		env.assertConformance(t, scenario)
	})
}

// prepareScenario resets the kernel tables, publishes the shared immutable
// AgentVersion, submits the task for this provider's runtime class, and runs
// admission plus scheduling. Each provider leg therefore observes the exact
// same published version.
func (env *conformanceEnv) prepareScenario(t *testing.T, scenario scenario) {
	t.Helper()
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx, `TRUNCATE TABLE runtime_operation_receipts, checkpoints, artifacts,
		agent_versions, inbox_receipts, outbox_events, runtime_leases, attempts, runs, tasks RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset database: %v", err)
	}
	published, err := env.store.CreateAgentVersion(ctx, kernelstore.CreateAgentVersionInput{
		ID: uuid.New(), TenantID: env.tenant, Namespace: "default",
		Name: "conformance", Version: "1",
		Spec: []byte(`{"runtimeClassPolicy":{"allowed":["oci","wasmtime"],"preferred":"oci"},"lifecycle":{"maxAttempts":3}}`),
	})
	if err != nil {
		t.Fatalf("publish conformance agent version: %v", err)
	}
	env.versionID = published.AgentVersion.ID
	created, err := env.store.CreateTask(ctx, kernelstore.CreateTaskInput{
		ID: uuid.New(), TenantID: env.tenant, Namespace: "default",
		AgentVersionRef: env.versionRef, Goal: "conformance goal " + scenario.key,
		IdempotencyKey: scenario.key, Spec: []byte(scenario.spec),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	env.taskID = created.Task.ID
	engine := admission.New(admission.Limits{
		RuntimeClasses: []string{"oci", "wasmtime"}, MaxTokens: 1000, MaxCostUSD: 10,
		MaxToolCalls: 100, MaxWallSeconds: 3600, MaxCPU: 2000, MaxMemory: 4096, MaxLLMConcurrency: 4,
	})
	admissionController := admission.NewController(env.store, engine, "admission-"+scenario.key, 10, time.Minute)
	if count, err := admissionController.Reconcile(ctx); err != nil || count != 1 {
		t.Fatalf("admission reconcile count=%d err=%v", count, err)
	}
	pools := staticPools{{
		ID: "pool-" + scenario.runtimeClass, TenantIDs: []string{env.tenant},
		RuntimeClass: scenario.runtimeClass, RuntimeInstanceID: scenario.instanceID,
		Region: "cn-east", DataResidency: "cn", Ready: true,
		AvailableCPU: 2000, AvailableMemory: 4096, AvailableLLMSlots: 4,
	}}
	schedulerController := scheduler.NewController(env.store, pools, "scheduler-"+scenario.key, 10, time.Minute, 30*time.Second)
	if count, err := schedulerController.Reconcile(ctx); err != nil || count != 1 {
		t.Fatalf("scheduler reconcile count=%d err=%v", count, err)
	}
	task, err := env.store.GetTask(ctx, env.tenant, env.taskID)
	if err != nil || task.Phase != domain.TaskRunning {
		t.Fatalf("task was not scheduled: %+v err=%v", task, err)
	}
	if task.AgentVersionID == nil || *task.AgentVersionID != env.versionID {
		t.Fatalf("task is not bound to the published version: %+v", task)
	}
}

// driveReferenceWorker runs the deterministic Go provider in-process until
// the task reaches a terminal phase.
func (env *conformanceEnv) driveReferenceWorker(t *testing.T) {
	t.Helper()
	artifacts, err := artifact.NewFilesystem(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatalf("create artifact store: %v", err)
	}
	connection, err := grpc.NewClient(env.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("connect to Runtime Protocol: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	worker := reference.NewWorker(
		runtimev1alpha1.NewRuntimeControlServiceClient(connection),
		artifacts, env.tenant, "worker-reference-1", 30*time.Second,
	)
	ctx := context.Background()
	deadline := time.Now().Add(terminalWaitTimeout)
	for {
		task, err := env.store.GetTask(ctx, env.tenant, env.taskID)
		if err != nil {
			t.Fatalf("read task: %v", err)
		}
		if task.Phase.Terminal() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reference provider did not finish: phase=%s", task.Phase)
		}
		done, err := worker.RunOnce(ctx)
		if err != nil {
			t.Fatalf("reference worker: %v", err)
		}
		if done {
			continue
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// driveWasmtimeWorker spawns the separate Wasmtime provider process. The
// binary and its package root are supplied through the environment so that
// CI can build them in the same job; locally they are built once with the
// pinned toolchain.
func (env *conformanceEnv) driveWasmtimeWorker(t *testing.T) {
	t.Helper()
	runtimeBinary := os.Getenv("AGENTOS_WASMTIME_RUNTIME")
	packageRoot := os.Getenv("AGENTOS_WASMTIME_PACKAGE_ROOT")
	if runtimeBinary == "" || packageRoot == "" {
		t.Skip("AGENTOS_WASMTIME_RUNTIME and AGENTOS_WASMTIME_PACKAGE_ROOT are not set")
	}
	if _, err := os.Stat(filepath.Join(packageRoot, "agent.wasm")); err != nil {
		t.Skipf("package root has no agent.wasm component: %v", err)
	}
	command := exec.Command(runtimeBinary,
		"--dev-mode", "true",
		"--tenant", env.tenant,
		"--runtime-instance-id", "worker-wasmtime-1",
		"--control-endpoint", env.grpcAddr,
		"--package-root", packageRoot,
		"--artifact-root", t.TempDir(),
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start Wasmtime provider: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		// The wait loop may have already consumed the exit value; only wait
		// when the process has not been observed exiting.
		select {
		case <-exited:
		default:
		}
	})

	ctx := context.Background()
	deadline := time.Now().Add(terminalWaitTimeout)
	for {
		select {
		case err := <-exited:
			t.Fatalf("Wasmtime provider exited early: %v\nstderr:\n%s", err, stderr.String())
		default:
		}
		task, err := env.store.GetTask(ctx, env.tenant, env.taskID)
		if err != nil {
			t.Fatalf("read task: %v", err)
		}
		if task.Phase.Terminal() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Wasmtime provider did not finish: phase=%s\nstderr:\n%s", task.Phase, stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// assertConformance verifies the kernel-visible invariants that both
// providers must satisfy identically for the same published AgentVersion.
func (env *conformanceEnv) assertConformance(t *testing.T, scenario scenario) {
	t.Helper()
	ctx := context.Background()

	task, err := env.store.GetTask(ctx, env.tenant, env.taskID)
	if err != nil {
		t.Fatalf("read final task: %v", err)
	}
	if task.Phase != domain.TaskSucceeded {
		t.Fatalf("task phase = %s, want SUCCEEDED", task.Phase)
	}
	if task.ResultRef == "" {
		t.Fatal("task has no durable result reference")
	}
	if task.AgentVersionID == nil || *task.AgentVersionID != env.versionID {
		t.Fatalf("task version binding = %v, want %s", task.AgentVersionID, env.versionID)
	}

	var runPhase, attemptPhase string
	if err := env.pool.QueryRow(ctx, `SELECT phase FROM runs WHERE task_id = $1`, env.taskID.String()).Scan(&runPhase); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if err := env.pool.QueryRow(ctx, `SELECT a.phase FROM attempts a JOIN runs r ON r.id = a.run_id
		WHERE r.task_id = $1`, env.taskID.String()).Scan(&attemptPhase); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if runPhase != string(domain.RunCompleted) || attemptPhase != string(domain.AttemptCompleted) {
		t.Fatalf("lifecycle did not converge: run=%s attempt=%s", runPhase, attemptPhase)
	}

	var checkpointCount int
	var checkpointProvider, checkpointABI, checkpointSchema string
	var checkpointVersionID *string
	if err := env.pool.QueryRow(ctx, `SELECT count(*), min(provider), min(runtime_abi), min(schema_version), min(agent_version_id::text)
		FROM checkpoints c JOIN runs r ON r.id = c.run_id WHERE r.task_id = $1`,
		env.taskID.String()).Scan(&checkpointCount, &checkpointProvider, &checkpointABI, &checkpointSchema, &checkpointVersionID); err != nil {
		t.Fatalf("read checkpoints: %v", err)
	}
	if checkpointCount != 1 || checkpointProvider != scenario.provider ||
		checkpointABI != scenario.runtimeABI || checkpointSchema != scenario.checkpoint {
		t.Fatalf("unexpected checkpoint: count=%d provider=%s abi=%s schema=%s", checkpointCount, checkpointProvider, checkpointABI, checkpointSchema)
	}
	if checkpointVersionID == nil || *checkpointVersionID != env.versionID.String() {
		t.Fatalf("checkpoint version binding = %v, want %s", checkpointVersionID, env.versionID)
	}

	var artifactCount int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM artifacts WHERE tenant_id = $1`, env.tenant).Scan(&artifactCount); err != nil {
		t.Fatalf("read artifacts: %v", err)
	}
	if artifactCount < 2 {
		t.Fatalf("artifacts = %d, want at least checkpoint state and result", artifactCount)
	}

	var succeededEvents int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events
		WHERE tenant_id = $1 AND aggregate_type = 'Task' AND aggregate_id = $2 AND event_type = 'TaskSucceeded'`,
		env.tenant, env.taskID.String()).Scan(&succeededEvents); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if succeededEvents != 1 {
		t.Fatalf("TaskSucceeded outbox events = %d, want 1", succeededEvents)
	}
}

func newConformanceEnv(t *testing.T) *conformanceEnv {
	t.Helper()
	url := os.Getenv("AGENTOS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGENTOS_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The conformance suite runs in its own PostgreSQL schema so that it can
	// truncate kernel tables without racing the store package's integration
	// tests, which share the same database.
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("open PostgreSQL admin connection: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS agentos_conformance`); err != nil {
		admin.Close(ctx)
		t.Fatalf("create conformance schema: %v", err)
	}
	if err := admin.Close(ctx); err != nil {
		t.Fatalf("close admin connection: %v", err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = "agentos_conformance"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	migrations := filepath.Join("..", "..", "..", "db", "migrations")
	if _, err := migrate.Apply(ctx, pool, migrations); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE runtime_operation_receipts, checkpoints, artifacts,
		agent_versions, inbox_receipts, outbox_events, runtime_leases, attempts, runs, tasks RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset database: %v", err)
	}
	repository := postgresstore.New(pool)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for Runtime Protocol: %v", err)
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(1<<20), grpc.MaxSendMsgSize(1<<20))
	runtimev1alpha1.RegisterRuntimeControlServiceServer(server,
		runtimecontrol.NewService(repository, conformanceTenant, 2*time.Minute))
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)

	return &conformanceEnv{
		pool: pool, store: repository, grpcAddr: listener.Addr().String(),
		tenant: conformanceTenant, versionRef: conformanceVersion,
	}
}

type staticPools []scheduler.RuntimePool

func (s staticPools) ListRuntimePools(context.Context, string) ([]scheduler.RuntimePool, error) {
	return s, nil
}
