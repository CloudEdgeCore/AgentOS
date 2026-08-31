//go:build linux && integration

package devops_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
)

// ociImageName is the containerd image name the OCI drill runs. The CI job
// builds deploy/oci/agent-runtime/Dockerfile and imports it before running
// this test.
const ociImageName = "docker.io/library/agentos-runtime:latest"

// requireOCIDrillEnvironment validates that the real gVisor/containerd
// environment is available; skips otherwise.
func requireOCIDrillEnvironment(t *testing.T) {
	t.Helper()
	if os.Getenv("AGENTOS_RUN_OCI_DRILL") != "1" {
		t.Skip("AGENTOS_RUN_OCI_DRILL is not set; the OCI/gVisor drill runs in its dedicated CI job")
	}
	// containerd CLI must be present.
	if _, err := exec.LookPath("ctr"); err != nil {
		t.Skipf("ctr (containerd CLI) not found: %v", err)
	}
	// The agent image must be imported into containerd.
	if out, err := exec.Command("ctr", "images", "get", ociImageName).CombinedOutput(); err != nil {
		t.Skipf("agent image %s not imported into containerd: %v\n%s", ociImageName, err, out)
	}
}

// TestOCIIsolation is the A-path isolation drill: a task whose spec targets
// an OCI container is placed on the `oci` pool, executed by the real
// agentos-runtime-oci worker inside a gVisor sandbox, and completes
// successfully.
func TestOCIIsolation(t *testing.T) {
	requireOCIDrillEnvironment(t)
	h := newHarness(t, "oci-iso", false)
	ctx := context.Background()

	// Publish an agent that runs on the oci class.
	ociAgentSpec, _ := json.Marshal(map[string]any{
		"runtimeClassPolicy": map[string]any{"allowed": []string{"oci", "research-network"}, "preferred": "oci"},
		"lifecycle":          map[string]any{"maxAttempts": 3},
		"runtimes": []any{
			map[string]any{"class": "oci", "interface": "agentos.runtime.interface/v1", "runtimeABI": "agentos.oci/v1", "entrypoint": []string{"oci://agent-runtime"}},
			map[string]any{"class": "research-network", "interface": "agentos.runtime.interface/v1", "runtimeABI": "agentos.adapter-http/v1", "entrypoint": []string{"agentos-binding://devops"}},
		},
		"capabilities": map[string]any{"tools": []string{"hello.echo@1.0.0"}, "models": []any{}, "memory": []any{}, "secrets": []any{}},
		"budget":       map[string]any{"tokens": 2000, "costUsd": 0.10, "toolCalls": 8, "wallSeconds": 120},
		"checkpoint":   map[string]any{"mode": "logical", "schemaVersion": "devops/v1", "intervalSeconds": 30},
	})
	if _, err := h.store.CreateAgentVersion(ctx, kernelstore.CreateAgentVersionInput{
		ID: uuid.New(), TenantID: devopsTenant, Namespace: "default",
		Name: "oci-agent", Version: "1.0.0", Spec: ociAgentSpec,
	}); err != nil {
		t.Fatalf("publish oci agent: %v", err)
	}

	// Build the OCI worker binary and start it as a subprocess.
	ociWorkerBin := filepath.Join("..", "..", "..", "..", "bin", "agentos-runtime-oci")
	if _, err := os.Stat(ociWorkerBin); err != nil {
		t.Skipf("agentos-runtime-oci binary not found at %s (build with: go build ./cmd/agentos-runtime-oci)", ociWorkerBin)
	}
	artifactRoot := t.TempDir()
	workerCmd := exec.Command(ociWorkerBin,
		"-control-address", h.listener.Addr().String(),
		"-tenant", devopsTenant,
		"-runtime-instance-id", "oci-worker-1",
		"-artifact-root", artifactRoot,
		"-image-ref", ociImageName,
		"-skip-image-pull",
	)
	workerCmd.Stderr = os.Stderr
	if err := workerCmd.Start(); err != nil {
		t.Fatalf("start oci worker: %v", err)
	}
	defer workerCmd.Process.Kill()
	t.Logf("oci worker started (pid %d)", workerCmd.Process.Pid)

	// Create a task targeting the oci pool.
	taskID := uuid.New()
	spec := map[string]any{
		"priority": 50,
		"budget":   map[string]any{"tokens": 1000, "costUsd": 0.10, "toolCalls": 4, "wallSeconds": 60},
		"placement": map[string]any{
			"runtimeClasses": []string{"oci"},
			"preferredClass": "oci",
			"region":         "cn-east",
			"cpuMillis":      250,
			"memoryMiB":      128,
			"workspaceBytes": 8388608,
			"llmConcurrency": 1,
		},
		"retryPolicy": map[string]any{"maxAttempts": 3},
	}
	specJSON, _ := json.Marshal(spec)
	if _, err := h.store.CreateTask(ctx, kernelstore.CreateTaskInput{
		ID: taskID, TenantID: devopsTenant, Namespace: "default",
		AgentVersionRef: "oci-agent@1.0.0", Goal: "oci isolation test",
		Spec: specJSON, IdempotencyKey: "oci-iso/" + taskID.String(),
	}); err != nil {
		t.Fatalf("create oci task: %v", err)
	}

	// Wait for terminal state.
	var phase string
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if err := h.pool.QueryRow(ctx, `SELECT phase FROM tasks WHERE id = $1`, taskID).Scan(&phase); err != nil {
			t.Fatalf("get task phase: %v", err)
		}
		if phase == "SUCCEEDED" || phase == "FAILED" || phase == "CANCELLED" || phase == "TIMED_OUT" || phase == "REJECTED" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if phase != "SUCCEEDED" {
		t.Fatalf("oci task phase = %s, want SUCCEEDED", phase)
	}

	// Verify the attempt ran on the oci worker.
	var attemptClass, attemptInstance string
	if err := h.pool.QueryRow(ctx,
		`SELECT a.runtime_class, a.runtime_instance_id FROM attempts a
		 JOIN runs r ON r.id = a.run_id AND r.tenant_id = a.tenant_id
		 WHERE r.task_id = $1 ORDER BY a.ordinal DESC LIMIT 1`,
		taskID).Scan(&attemptClass, &attemptInstance); err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if attemptClass != "oci" || attemptInstance != "oci-worker-1" {
		t.Fatalf("attempt = %s/%s, want oci/oci-worker-1", attemptClass, attemptInstance)
	}
	t.Logf("OCI isolation verified: task SUCCEEDED on %s (%s) inside gVisor sandbox", attemptInstance, attemptClass)
}

// TestOCITakeover proves cross-runtime-class takeover from the OCI/gVisor
// pool to an adapter pool: the oci pool is cordoned and its worker crashes,
// and the kernel re-places the task onto an adapter pool.
func TestOCITakeover(t *testing.T) {
	requireOCIDrillEnvironment(t)
	h := newHarness(t, "oci-takeover", false)
	ctx := context.Background()

	// Dual-class agent: oci (container) + network (adapter).
	dualSpec, _ := json.Marshal(map[string]any{
		"runtimeClassPolicy": map[string]any{
			"allowed": []string{"oci", "research-network"}, "preferred": "oci",
		},
		"lifecycle": map[string]any{"maxAttempts": 3},
		"runtimes": []any{
			map[string]any{"class": "oci", "interface": "agentos.runtime.interface/v1", "runtimeABI": "agentos.oci/v1", "entrypoint": []string{"oci://agent-runtime"}},
			map[string]any{"class": "research-network", "interface": "agentos.runtime.interface/v1", "runtimeABI": "agentos.adapter-http/v1", "entrypoint": []string{"agentos-binding://devops"}},
		},
		"capabilities": map[string]any{"tools": []string{"hello.echo@1.0.0"}, "models": []any{}, "memory": []any{}, "secrets": []any{}},
		"budget":       map[string]any{"tokens": 2000, "costUsd": 0.10, "toolCalls": 8, "wallSeconds": 120},
		"checkpoint":   map[string]any{"mode": "logical", "schemaVersion": "devops/v1", "intervalSeconds": 30},
	})
	if _, err := h.store.CreateAgentVersion(ctx, kernelstore.CreateAgentVersionInput{
		ID: uuid.New(), TenantID: devopsTenant, Namespace: "default",
		Name: "oci-dual-agent", Version: "1.0.0", Spec: dualSpec,
	}); err != nil {
		t.Fatalf("publish dual agent: %v", err)
	}

	ociWorkerBin := filepath.Join("..", "..", "..", "..", "bin", "agentos-runtime-oci")
	if _, err := os.Stat(ociWorkerBin); err != nil {
		t.Skipf("agentos-runtime-oci binary not found")
	}
	workerCmd := exec.Command(ociWorkerBin,
		"-control-address", h.listener.Addr().String(),
		"-tenant", devopsTenant,
		"-runtime-instance-id", "oci-worker-1",
		"-artifact-root", t.TempDir(),
		"-image-ref", ociImageName,
		"-skip-image-pull",
	)
	workerCmd.Stderr = os.Stderr
	if err := workerCmd.Start(); err != nil {
		t.Fatalf("start oci worker: %v", err)
	}
	defer workerCmd.Process.Kill()

	taskID := uuid.New()
	spec := map[string]any{
		"priority": 50,
		"budget":   map[string]any{"tokens": 2000, "costUsd": 0.10, "toolCalls": 8, "wallSeconds": 120},
		"placement": map[string]any{
			"runtimeClasses": []string{"oci", "research-network"},
			"preferredClass": "oci",
			"region":         "cn-east",
			"cpuMillis":      250,
			"memoryMiB":      128,
			"workspaceBytes": 8388608,
			"llmConcurrency": 1,
		},
		"retryPolicy": map[string]any{"maxAttempts": 3},
	}
	specJSON, _ := json.Marshal(spec)
	if _, err := h.store.CreateTask(ctx, kernelstore.CreateTaskInput{
		ID: taskID, TenantID: devopsTenant, Namespace: "default",
		AgentVersionRef: "oci-dual-agent@1.0.0", Goal: "oci takeover test",
		Spec: specJSON, IdempotencyKey: "oci-takeover/" + taskID.String(),
	}); err != nil {
		t.Fatalf("create takeover task: %v", err)
	}

	// Wait until placed on the oci worker.
	deadline := time.Now().Add(90 * time.Second)
	for {
		var placed int
		if err := h.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM attempts a
			 JOIN runs r ON r.id = a.run_id AND r.tenant_id = a.tenant_id
			 WHERE r.task_id = $1 AND a.runtime_instance_id = 'oci-worker-1'`,
			taskID).Scan(&placed); err != nil {
			t.Fatalf("count oci attempts: %v", err)
		}
		if placed >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never placed on oci worker within 90s")
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Logf("task placed on oci worker (gVisor)")

	// Cordon the oci pool + kill the worker → re-placement to adapter.
	h.cordonPool("devops-pool-5")
	_ = workerCmd.Process.Kill()

	// Wait for terminal.
	var phase string
	recoverDeadline := time.Now().Add(2 * time.Minute)
	for {
		if err := h.pool.QueryRow(ctx, `SELECT phase FROM tasks WHERE id = $1`, taskID).Scan(&phase); err != nil {
			t.Fatalf("get phase: %v", err)
		}
		if phase == "SUCCEEDED" || phase == "FAILED" {
			break
		}
		if time.Now().After(recoverDeadline) {
			t.Fatalf("oci takeover did not converge (phase=%s)", phase)
		}
		h.pool.Exec(ctx, `
			UPDATE runtime_leases l SET expires_at = l.acquired_at + INTERVAL '1 microsecond'
			FROM attempts a
			JOIN runs r ON r.id = a.run_id AND r.tenant_id = a.tenant_id
			WHERE a.id = l.attempt_id AND r.task_id = $1 AND l.released_at IS NULL`, taskID)
		time.Sleep(200 * time.Millisecond)
	}

	// Verify the final attempt is NOT on the oci worker.
	var finalClass, finalInstance string
	if err := h.pool.QueryRow(ctx,
		`SELECT a.runtime_class, a.runtime_instance_id FROM attempts a
		 JOIN runs r ON r.id = a.run_id AND r.tenant_id = a.tenant_id
		 WHERE r.task_id = $1 ORDER BY a.ordinal DESC LIMIT 1`,
		taskID).Scan(&finalClass, &finalInstance); err != nil {
		t.Fatalf("get final attempt: %v", err)
	}
	if finalClass == "oci" {
		t.Fatalf("takeover failed: final attempt still on oci (%s)", finalInstance)
	}
	if phase != "SUCCEEDED" {
		t.Fatalf("takeover: task phase = %s, want SUCCEEDED", phase)
	}
	t.Logf("OCI takeover verified: gVisor(oci) → %s (%s), task SUCCEEDED", finalClass, finalInstance)
}

var _ = strings.TrimSpace
