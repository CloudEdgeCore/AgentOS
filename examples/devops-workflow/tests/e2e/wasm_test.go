//go:build integration

package devops_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
)

// TestWasmtimeIsolation is the B-path isolation drill: a task whose spec
// targets a Wasmtime component is placed on a `research-wasm` pool, executed
// by the real agentos-runtime-wasm worker inside a Wasmtime sandbox, and
// completes successfully. Proves that the kernel dispatches to real
// Wasmtime isolation (not just adapter labels).

// resolveBinary resolves a tool binary, appending the platform extension when
// present (Windows executables carry .exe; CI is Linux).
func resolveBinary(t *testing.T, base string) string {
	t.Helper()
	plain := base
	if _, err := os.Stat(plain); err == nil {
		return plain
	}
	withExt := base + ".exe"
	if _, err := os.Stat(withExt); err == nil {
		return withExt
	}
	t.Skipf("binary not found at %s or %s (build with cargo build --release)", plain, withExt)
	return ""
}

func TestWasmtimeIsolation(t *testing.T) {
	h := newHarness(t, "wasm-iso", false)
	ctx := context.Background()

	// Generate the WASM conformance component.
	binPath := resolveBinary(t, filepath.Join("..", "..", "..", "..", "target", "release", "component-fixture"))
	packageRoot := t.TempDir()
	wasmPath := filepath.Join(packageRoot, "hello.wasm")
	cmd := exec.Command(binPath, "--out", wasmPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate wasm component: %v\n%s", err, out)
	}

	// Start the real Wasmtime worker as a subprocess.
	workerBin := resolveBinary(t, filepath.Join("..", "..", "..", "..", "target", "release", "agentos-runtime-wasm"))
	artifactRoot := t.TempDir()
	controlAddr := h.listener.Addr().String()
	workerCmd := exec.Command(workerBin,
		"--control-endpoint", controlAddr,
		"--tenant", devopsTenant,
		"--runtime-instance-id", "wasm-worker-1",
		"--package-root", packageRoot,
		"--artifact-root", artifactRoot,
		"--dev-mode", "true",
	)
	workerCmd.Stderr = os.Stderr
	if err := workerCmd.Start(); err != nil {
		t.Fatalf("start wasm worker: %v", err)
	}
	defer workerCmd.Process.Kill()
	t.Logf("wasm worker started (pid %d, control %s, package %s)", workerCmd.Process.Pid, controlAddr, packageRoot)

	// Publish a dual-class agent (wasm + adapter) so the task can be placed
	// on the wasm pool without needing adapter worker bindings.
	dualAgentSpec := map[string]any{
		"runtimeClassPolicy": map[string]any{
			"allowed":   []string{"research-wasm", "research-network"},
			"preferred": "research-wasm",
		},
		"lifecycle": map[string]any{"maxAttempts": 3},
		"runtimes": []any{
			map[string]any{"class": "research-wasm", "interface": "agentos.runtime.interface/v1", "runtimeABI": "agentos.wasm-component/v1", "entrypoint": []string{"agentos-binding://devops"}},
		},
		"capabilities": map[string]any{"tools": []any{}, "models": []any{}, "memory": []any{}, "secrets": []any{}},
		"budget":       map[string]any{"tokens": 1000, "costUsd": 0.10, "toolCalls": 4, "wallSeconds": 60},
		"checkpoint":   map[string]any{"mode": "logical", "schemaVersion": "devops/v1", "intervalSeconds": 30},
	}
	daSpec, _ := json.Marshal(dualAgentSpec)
	if _, err := h.store.CreateAgentVersion(ctx, kernelstore.CreateAgentVersionInput{
		ID: uuid.New(), TenantID: devopsTenant, Namespace: "default",
		Name: "wasm-only-agent", Version: "1.0.0", Spec: daSpec,
	}); err != nil {
		t.Fatalf("publish wasm-only agent: %v", err)
	}

	// Create a task that targets the wasm pool.
	taskID := uuid.New()
	spec := map[string]any{
		"priority": 50,
		"budget":   map[string]any{"tokens": 1000, "costUsd": 0.10, "toolCalls": 4, "wallSeconds": 60},
		"placement": map[string]any{
			"runtimeClasses": []string{"research-wasm"},
			"preferredClass": "research-wasm",
			"region":         "cn-east",
			"cpuMillis":      250,
			"memoryMiB":      256,
			"workspaceBytes": 8388608,
			"llmConcurrency": 1,
		},
		"runtime": map[string]any{
			"componentPath": "hello.wasm",
		},
		"retryPolicy": map[string]any{"maxAttempts": 3},
	}
	specJSON, _ := json.Marshal(spec)
	if _, err := h.store.CreateTask(ctx, kernelstore.CreateTaskInput{
		ID: taskID, TenantID: devopsTenant, Namespace: "default",
		AgentVersionRef: "wasm-only-agent@1.0.0", Goal: "wasm isolation test",
		Spec: specJSON, IdempotencyKey: "wasm-iso/" + taskID.String(),
	}); err != nil {
		t.Fatalf("create wasm task: %v", err)
	}

	// Wait for terminal state.
	var phase string
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if err := h.pool.QueryRow(ctx,
			`SELECT phase FROM tasks WHERE id = $1`, taskID).Scan(&phase); err != nil {
			t.Fatalf("get task phase: %v", err)
		}
		if phase == "SUCCEEDED" || phase == "FAILED" || phase == "CANCELLED" || phase == "TIMED_OUT" || phase == "REJECTED" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if phase != "SUCCEEDED" {
		t.Fatalf("wasm task phase = %s, want SUCCEEDED", phase)
	}

	// Verify the attempt ran on the wasm worker with the wasm runtime class.
	var attemptClass, attemptInstance string
	if err := h.pool.QueryRow(ctx,
		`SELECT a.runtime_class, a.runtime_instance_id FROM attempts a
		 JOIN runs r ON r.id = a.run_id AND r.tenant_id = a.tenant_id
		 WHERE r.task_id = $1 ORDER BY a.ordinal DESC LIMIT 1`,
		taskID).Scan(&attemptClass, &attemptInstance); err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if attemptClass != "research-wasm" {
		t.Fatalf("attempt class = %s, want research-wasm", attemptClass)
	}
	if attemptInstance != "wasm-worker-1" {
		t.Fatalf("attempt instance = %s, want wasm-worker-1", attemptInstance)
	}
	t.Logf("Wasmtime isolation verified: task SUCCEEDED on %s (%s) inside Wasmtime sandbox", attemptInstance, attemptClass)
}

// TestWasmtimeTakeover proves cross-runtime-class takeover: a task starts on
// the Wasmtime pool, the pool is cordoned and the worker crashes, and the
// kernel re-places it onto an adapter pool.
func TestWasmtimeTakeover(t *testing.T) {
	h := newHarness(t, "wasm-takeover", false)
	ctx := context.Background()

	binPath := resolveBinary(t, filepath.Join("..", "..", "..", "..", "target", "release", "component-fixture"))
	packageRoot := t.TempDir()
	wasmPath := filepath.Join(packageRoot, "hello.wasm")
	if out, err := exec.Command(binPath, "--out", wasmPath).CombinedOutput(); err != nil {
		t.Fatalf("generate wasm: %v\n%s", err, out)
	}
	workerBin := resolveBinary(t, filepath.Join("..", "..", "..", "..", "target", "release", "agentos-runtime-wasm"))

	// Publish a dual-class agent (wasm + adapter) so the task can be
	// re-placed across runtime boundaries.
	dualAgentManifest := map[string]any{
		"apiVersion": "agentos.dev/v1", "kind": "AgentVersion",
		"metadata": map[string]any{"name": "dual-class-agent", "version": "1.0.0", "namespace": "default"},
		"spec": map[string]any{
			"runtimeClassPolicy": map[string]any{
				"allowed":   []string{"research-wasm", "research-network"},
				"preferred": "research-wasm",
			},
			"lifecycle": map[string]any{"maxAttempts": 3},
			"runtimes": []any{
				map[string]any{"class": "research-wasm", "interface": "agentos.runtime.interface/v1", "runtimeABI": "agentos.wasm-component/v1", "entrypoint": []string{"agentos-binding://devops"}},
				map[string]any{"class": "research-network", "interface": "agentos.runtime.interface/v1", "runtimeABI": "agentos.adapter-http/v1", "entrypoint": []string{"agentos-binding://devops"}},
			},
			"capabilities": map[string]any{"tools": []string{"hello.echo@1.0.0"}, "models": []any{}, "memory": []any{}, "secrets": []any{}},
			"budget":       map[string]any{"tokens": 2000, "costUsd": 0.10, "toolCalls": 8, "wallSeconds": 120},
			"checkpoint":   map[string]any{"mode": "logical", "schemaVersion": "devops/v1", "intervalSeconds": 30},
		},
	}
	specBytes, _ := json.Marshal(dualAgentManifest["spec"])
	if _, err := h.store.CreateAgentVersion(ctx, kernelstore.CreateAgentVersionInput{
		ID: uuid.New(), TenantID: devopsTenant, Namespace: "default",
		Name: "dual-class-agent", Version: "1.0.0", Spec: specBytes,
	}); err != nil {
		t.Fatalf("publish dual-class agent: %v", err)
	}

	artifactRoot := t.TempDir()
	workerCmd := exec.Command(workerBin,
		"--control-endpoint", h.listener.Addr().String(),
		"--tenant", devopsTenant,
		"--runtime-instance-id", "wasm-worker-1",
		"--package-root", packageRoot,
		"--artifact-root", artifactRoot,
		"--dev-mode", "true",
	)
	workerCmd.Stderr = os.Stderr
	if err := workerCmd.Start(); err != nil {
		t.Fatalf("start wasm worker: %v", err)
	}
	defer workerCmd.Process.Kill()

	// Create a task that starts on wasm, can fall back to adapter.
	taskID := uuid.New()
	spec := map[string]any{
		"priority": 50,
		"budget":   map[string]any{"tokens": 2000, "costUsd": 0.10, "toolCalls": 8, "wallSeconds": 120},
		"placement": map[string]any{
			"runtimeClasses": []string{"research-wasm", "research-network"},
			"preferredClass": "research-wasm",
			"region":         "cn-east",
			"cpuMillis":      250,
			"memoryMiB":      256,
			"workspaceBytes": 8388608,
			"llmConcurrency": 1,
		},
		"runtime":     map[string]any{"componentPath": "hello.wasm"},
		"retryPolicy": map[string]any{"maxAttempts": 3},
	}
	specJSON, _ := json.Marshal(spec)
	if _, err := h.store.CreateTask(ctx, kernelstore.CreateTaskInput{
		ID: taskID, TenantID: devopsTenant, Namespace: "default",
		AgentVersionRef: "dual-class-agent@1.0.0", Goal: "wasm takeover test",
		Spec: specJSON, IdempotencyKey: "wasm-takeover/" + taskID.String(),
	}); err != nil {
		t.Fatalf("create takeover task: %v", err)
	}

	// Wait until placed on the wasm worker.
	deadline := time.Now().Add(60 * time.Second)
	for {
		var placed int
		if err := h.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM attempts a
			 JOIN runs r ON r.id = a.run_id AND r.tenant_id = a.tenant_id
			 WHERE r.task_id = $1 AND a.runtime_instance_id = 'wasm-worker-1'`,
			taskID).Scan(&placed); err != nil {
			t.Fatalf("count wasm attempts: %v", err)
		}
		if placed >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never placed on wasm worker within 60s")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("task placed on wasm worker")

	// Cordon the wasm pool + kill the worker → re-placement to adapter.
	h.cordonPool("devops-pool-4") // wasm pool
	workerCmd.Process.Kill()

	// Wait for terminal.
	var phase string
	recoverDeadline := time.Now().Add(90 * time.Second)
	for {
		if err := h.pool.QueryRow(ctx, `SELECT phase FROM tasks WHERE id = $1`, taskID).Scan(&phase); err != nil {
			t.Fatalf("get phase: %v", err)
		}
		if phase == "SUCCEEDED" || phase == "FAILED" {
			break
		}
		if time.Now().After(recoverDeadline) {
			t.Fatalf("wasm takeover did not converge (phase=%s)", phase)
		}
		h.pool.Exec(ctx, `
			UPDATE runtime_leases l SET expires_at = l.acquired_at + INTERVAL '1 microsecond'
			FROM attempts a
			JOIN runs r ON r.id = a.run_id AND r.tenant_id = a.tenant_id
			WHERE a.id = l.attempt_id AND r.task_id = $1 AND l.released_at IS NULL`, taskID)
		time.Sleep(200 * time.Millisecond)
	}

	// Verify the final attempt ran on a DIFFERENT runtime class.
	var finalClass, finalInstance string
	if err := h.pool.QueryRow(ctx,
		`SELECT a.runtime_class, a.runtime_instance_id FROM attempts a
		 JOIN runs r ON r.id = a.run_id AND r.tenant_id = a.tenant_id
		 WHERE r.task_id = $1 ORDER BY a.ordinal DESC LIMIT 1`,
		taskID).Scan(&finalClass, &finalInstance); err != nil {
		t.Fatalf("get final attempt: %v", err)
	}
	if finalClass == "research-wasm" {
		t.Fatalf("takeover failed: final attempt still on wasm (%s)", finalInstance)
	}
	if phase != "SUCCEEDED" {
		t.Fatalf("takeover: task phase = %s, want SUCCEEDED", phase)
	}
	t.Logf("wasm takeover verified: wasm → %s (%s), task SUCCEEDED", finalClass, finalInstance)
}
