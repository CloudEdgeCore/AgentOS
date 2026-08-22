//go:build integration

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/domain"
	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
)

// TestV11SingleAgentRealLoop is the Phase 1 headline acceptance: one real
// Python agent runs fully under AgentOS — manifest-published, scheduled,
// model-invoked through the OpenAI-compatible provider layer, tool-called
// through the MCP-mediated Tool Gateway, memory read/written, checkpointed,
// audited — and the exactness invariants hold (usage settled once, cost from
// the pinned price table, credential never observable by the agent).
func TestV11SingleAgentRealLoop(t *testing.T) {
	env := newE2EEnv(t, "agentos_e2e_single", 0, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stack := startStack(t, env)
	worker := makeWorker(t, env, stack, "worker-single-1", 30*time.Second)

	publishVersion(ctx, t, env, stack.agentURL)
	goal := "Report the weather. city:paris"
	task := submitTask(ctx, t, env, "single-real-1", goal)
	reconcile(ctx, t, env, poolsFor("worker-single-1"), false)

	go driveWorker(ctx, t, worker, "worker-single-1")
	finished := waitForTerminal(ctx, t, env, task.ID, 2*time.Minute)
	if finished.Phase != domain.TaskSucceeded {
		t.Fatalf("task phase = %s, want SUCCEEDED", finished.Phase)
	}

	// The result document carries the agent's real answer and usage.
	result := readResultArtifact(t, env, finished.ResultRef)
	output, _ := result["output"].(map[string]any)
	if output == nil || !strings.Contains(fmt.Sprint(output["answer"]), "weather acquired for paris") {
		t.Fatalf("result output = %#v", result["output"])
	}
	if output["resumed"] != false || len(asList(output["toolCalls"])) != 1 {
		t.Fatalf("result meta = %#v", output)
	}

	// Model ledger: two completed invocations with the exact scripted usage.
	var modelCalls int
	var totalInput, totalOutput int64
	var requestIDs []string
	rows, err := env.pool.Query(ctx, `SELECT status, input_tokens, output_tokens, provider_request_id FROM model_calls
		WHERE tenant_id = $1 AND task_id = $2`, e2eTenant, task.ID)
	if err != nil {
		t.Fatalf("read model calls: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var requestID string
		var input, output int64
		if err := rows.Scan(&status, &input, &output, &requestID); err != nil {
			t.Fatalf("scan model call: %v", err)
		}
		if status != "COMPLETED" {
			t.Fatalf("model call status = %s", status)
		}
		totalInput += input
		totalOutput += output
		modelCalls++
		requestIDs = append(requestIDs, requestID)
	}
	if modelCalls != 2 || totalInput != 100 || totalOutput != 50 {
		t.Fatalf("model calls = %d usage = %d/%d, want 2 calls with exact 100/50", modelCalls, totalInput, totalOutput)
	}
	if requestIDs[0] == "" || requestIDs[1] == "" || requestIDs[0] == requestIDs[1] {
		t.Fatalf("provider request ids = %v, want two distinct non-empty ids", requestIDs)
	}

	// Budget: the task settled exactly the model usage, once.
	settled := queryInt(ctx, t, env, `SELECT COALESCE(SUM(tokens),0) FROM task_budget_settlements WHERE task_id = $1`, task.ID)
	if settled != 150 {
		t.Fatalf("settled tokens = %d, want exactly 150", settled)
	}
	toolSettled := queryInt(ctx, t, env, `SELECT COALESCE(SUM(tool_calls),0) FROM task_budget_settlements WHERE task_id = $1`, task.ID)
	if toolSettled != 1 {
		t.Fatalf("settled tool calls = %d, want 1", toolSettled)
	}

	// Tool: the weather webhook really executed exactly once for the goal city.
	if calls := env.webhook.count("paris"); calls != 1 {
		t.Fatalf("weather webhook calls = %d, want 1", calls)
	}
	toolRows := queryInt(ctx, t, env, `SELECT count(*) FROM tool_calls WHERE task_id = $1 AND status = 'EXECUTED'`, task.ID)
	if toolRows != 1 {
		t.Fatalf("executed tool calls = %d, want 1", toolRows)
	}

	// Memory: the run summary landed in the tenant store.
	memories := queryInt(ctx, t, env, `SELECT count(*) FROM memory_records WHERE tenant_id = $1 AND namespace = 'runs'`, e2eTenant)
	if memories != 1 {
		t.Fatalf("memory records = %d, want 1 (agent-written run summary)", memories)
	}

	// Audit: model and tool receipts exist (metadata only).
	receipts := queryInt(ctx, t, env, `SELECT count(*) FROM runtime_operation_receipts r
		JOIN attempts a ON a.id = r.attempt_id JOIN runs rn ON rn.id = a.run_id
		WHERE rn.task_id = $1 AND (r.operation = 'MODEL:fake/agent-model' OR r.operation = 'TOOL:weather.lookup@1.0.0')`, task.ID)
	if receipts != 3 {
		t.Fatalf("audit receipts = %d, want 3 (2 model + 1 tool)", receipts)
	}

	// Credential isolation: the provider key appears in the outbound
	// Authorization headers but nowhere in any agent-visible artifact.
	for _, authorization := range env.provider.authorizations() {
		if !strings.Contains(authorization, e2eProviderKey) {
			t.Fatalf("provider saw authorization %q", authorization)
		}
	}
	if strings.Contains(resultDocument(t, env, finished), e2eProviderKey) {
		t.Fatalf("result document leaks the provider credential")
	}
	if strings.Contains(stack.pythonOut.String(), e2eProviderKey) {
		t.Fatalf("python agent stderr leaks the provider credential")
	}
}

// TestV11ThousandTaskPipeline is the Phase 1 scale acceptance:
// AGENTOS_E2E_TASKS (default 1000) complete tasks through the real loop with
// ≥99% success, concurrent workers sharing one agent endpoint, no duplicate
// settlements and no duplicate results.
func TestV11ThousandTaskPipeline(t *testing.T) {
	total := e2eCount("AGENTOS_E2E_TASKS", 1000)
	env := newE2EEnv(t, "agentos_e2e_scale", 0, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stack := startStack(t, env)
	instances := []string{"worker-scale-1", "worker-scale-2", "worker-scale-3", "worker-scale-4"}
	for _, instance := range instances {
		go driveWorker(ctx, t, makeWorker(t, env, stack, instance, 30*time.Second), instance)
	}
	pools := poolsFor(instances...)
	publishVersion(ctx, t, env, stack.agentURL)

	started := time.Now()
	go func() {
		for ctx.Err() == nil {
			reconcileQuiet(ctx, env, pools)
			time.Sleep(25 * time.Millisecond)
		}
	}()

	// Staged submission: a bounded in-flight window keeps every placed
	// attempt claimable within its lease — a real fleet is fed at capacity,
	// not with an unbounded burst that outlives the placement lease.
	const window = 64
	succeeded, failed := 0, 0
	submitted := 0
	pending := make([]uuid.UUID, 0, window)
	for submitted < total || len(pending) > 0 {
		for len(pending) < window && submitted < total {
			created := submitTask(ctx, t, env, fmt.Sprintf("scale-%d", submitted),
				fmt.Sprintf("Report the weather. city:city-%d", submitted%64))
			pending = append(pending, created.ID)
			submitted++
		}
		task := waitForTerminal(ctx, t, env, pending[0], 15*time.Minute)
		if task.Phase == domain.TaskSucceeded {
			succeeded++
		} else {
			failed++
			t.Logf("task %s ended in %s", task.ID, task.Phase)
		}
		pending = pending[1:]
	}
	elapsed := time.Since(started)

	// Exactness: total settled tokens equal 150 per succeeded task.
	expected := int64(succeeded) * 150
	settled := queryInt(ctx, t, env, `SELECT COALESCE(SUM(tokens),0) FROM task_budget_settlements WHERE tenant_id = $1`, e2eTenant)
	if settled != expected {
		t.Fatalf("settled tokens = %d, want exactly %d (150 per succeeded task)", settled, expected)
	}
	// Result uniqueness: no task result artifact is referenced twice.
	duplicates := queryInt(ctx, t, env, `SELECT count(*) FROM (
		SELECT result_ref FROM tasks WHERE result_ref IS NOT NULL GROUP BY result_ref HAVING count(*) > 1) d`)
	if duplicates != 0 {
		t.Fatalf("duplicate result references: %d", duplicates)
	}

	successRate := float64(succeeded) / float64(total)
	t.Logf("v1.1 scale evidence: tasks=%d succeeded=%d failed=%d successRate=%.4f elapsed=%s throughput=%.1f tasks/s",
		total, succeeded, failed, successRate, elapsed, float64(total)/elapsed.Seconds())
	if successRate < 0.99 {
		t.Logf("python runtime diagnostics: %s", stack.pythonOut.String())
		t.Fatalf("success rate %.4f below the 99%% acceptance gate", successRate)
	}
}

// TestV11RecoveryFaultInjection is the Phase 1.5 acceptance: the worker is
// killed mid-execution, the lease expires, the recovery controller requeues,
// and a fresh worker restores from the checkpoint — with the confirmed tool
// side effect never repeated and the task settling exactly once.
func TestV11RecoveryFaultInjection(t *testing.T) {
	faults := e2eCount("AGENTOS_E2E_FAULTS", 100)
	// The slow provider keeps executions alive long enough for the 1s
	// periodic checkpoint to confirm the tool turn before the kill.
	env := newE2EEnv(t, "agentos_e2e_fault", 700*time.Millisecond, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stack := startStack(t, env)
	instances := []string{"worker-fault-1", "worker-fault-2", "worker-fault-3", "worker-fault-4"}

	// Worker fleet with per-instance lifecycles: a fault kills one instance
	// and immediately respawns it to take over the recovered attempt.
	fleet := newWorkerFleet(t, env, stack, instances...)
	pools := poolsFor(instances...)
	publishVersion(ctx, t, env, stack.agentURL)

	go func() {
		for ctx.Err() == nil {
			reconcileQuiet(ctx, env, pools)
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// Fault rounds run concurrently across the fleet; each round gets a
	// unique city so webhook counts attribute exactly.
	rounds := make(chan int, faults)
	for i := 0; i < faults; i++ {
		rounds <- i
	}
	close(rounds)
	results := make(chan bool, faults)
	var wg sync.WaitGroup
	for range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range rounds {
				results <- injectOneFault(ctx, t, env, fleet, index)
			}
		}()
	}
	wg.Wait()
	close(results)

	succeeded := 0
	for ok := range results {
		if ok {
			succeeded++
		}
	}
	rate := float64(succeeded) / float64(faults)
	t.Logf("v1.1 recovery evidence: injections=%d recovered=%d rate=%.4f", faults, succeeded, rate)
	if rate < 0.99 {
		t.Fatalf("recovery success rate %.4f below the 99%% acceptance gate", rate)
	}

	// Global exactness across every injection: no duplicated results.
	duplicates := queryInt(ctx, t, env, `SELECT count(*) FROM (
		SELECT result_ref FROM tasks WHERE result_ref IS NOT NULL GROUP BY result_ref HAVING count(*) > 1) d`)
	if duplicates != 0 {
		t.Fatalf("duplicate final results: %d", duplicates)
	}
}

// injectOneFault runs one kill/recover cycle and reports success.
func injectOneFault(ctx context.Context, t *testing.T, env *e2eEnv, fleet *workerFleet, index int) bool {
	city := fmt.Sprintf("city-f%d", index)
	task := submitTask(ctx, t, env, fmt.Sprintf("fault-%d", index), "Report the weather. city:"+city)

	// Wait until a checkpoint confirmed the tool turn, then kill whichever
	// worker holds the attempt (its lease stops renewing; the attempt is
	// stranded RUNNING — the real crash semantics).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if confirmedToolCheckpoint(ctx, t, env, task.ID) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if instance := executingInstance(ctx, t, env, task.ID); instance != "" {
		fleet.killAndRespawn(instance)
	}

	finished := waitForTerminal(ctx, t, env, task.ID, 2*time.Minute)
	if finished.Phase != domain.TaskSucceeded {
		attempts := queryInt(ctx, t, env, `SELECT count(*) FROM attempts a JOIN runs r ON r.id = a.run_id WHERE r.task_id = $1`, task.ID)
		t.Errorf("fault %d: task phase = %s (attempts=%d), want SUCCEEDED", index, finished.Phase, attempts)
		return false
	}
	// The confirmed tool side effect happened exactly once.
	if calls := env.webhook.count(city); calls != 1 {
		t.Errorf("fault %d: weather webhook executed %d times for %s, want exactly 1", index, calls, city)
		return false
	}
	// Exactly one completed attempt owns the final result.
	completed := queryInt(ctx, t, env, `SELECT count(*) FROM attempts a JOIN runs r ON r.id = a.run_id
		WHERE r.task_id = $1 AND a.phase = 'COMPLETED'`, task.ID)
	if completed != 1 {
		t.Errorf("fault %d: completed attempts = %d, want 1", index, completed)
		return false
	}
	return true
}

// workerFleet manages the per-instance worker lifecycles of the fault test.
type workerFleet struct {
	t     *testing.T
	env   *e2eEnv
	stack *e2eStack

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func newWorkerFleet(t *testing.T, env *e2eEnv, stack *e2eStack, instances ...string) *workerFleet {
	t.Helper()
	fleet := &workerFleet{t: t, env: env, stack: stack, cancels: map[string]context.CancelFunc{}}
	for _, instance := range instances {
		fleet.spawn(instance)
	}
	return fleet
}

func (f *workerFleet) spawn(instance string) {
	workerCtx, cancel := context.WithCancel(context.Background())
	f.t.Cleanup(cancel)
	f.mu.Lock()
	f.cancels[instance] = cancel
	f.mu.Unlock()
	go driveWorker(workerCtx, f.t, makeWorker(f.t, f.env, f.stack, instance, 2*time.Second), instance)
}

// killAndRespawn cancels the instance's worker and starts a fresh one on the
// same identity — the replacement picks up the recovered attempt.
func (f *workerFleet) killAndRespawn(instance string) {
	f.mu.Lock()
	cancel := f.cancels[instance]
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	f.spawn(instance)
}

func readResultArtifact(t *testing.T, env *e2eEnv, reference string) map[string]any {
	t.Helper()
	document := resultDocument(t, env, kernelstore.Task{ResultRef: reference})
	var result map[string]any
	if err := json.Unmarshal([]byte(document), &result); err != nil {
		t.Fatalf("decode result document: %v", err)
	}
	return result
}

func resultDocument(t *testing.T, env *e2eEnv, task kernelstore.Task) string {
	t.Helper()
	var digest []byte
	var size int64
	var mediaType string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT sha256, size_bytes, media_type FROM artifacts WHERE uri = $1`, task.ResultRef).
		Scan(&digest, &size, &mediaType); err != nil {
		t.Fatalf("read result artifact metadata: %v", err)
	}
	stored := kernelstore.ArtifactReference{URI: task.ResultRef, MediaType: mediaType, SizeBytes: size}
	copy(stored.SHA256[:], digest)
	reader, err := env.artifacts.Open(context.Background(), e2eTenant, stored)
	if err != nil {
		t.Fatalf("open result artifact: %v", err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, 4<<20))
	if err != nil {
		t.Fatalf("read result artifact: %v", err)
	}
	return string(raw)
}

func asList(value any) []any {
	list, _ := value.([]any)
	return list
}
