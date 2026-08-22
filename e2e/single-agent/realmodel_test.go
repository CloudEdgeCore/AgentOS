//go:build integration

// Real-model smoke: exercises the v1.1 OpenAI-compatible execution layer
// against a live self-hosted endpoint (no PostgreSQL needed). Point it at
// your deployment:
//
//	AGENTOS_REAL_MODEL_URL=http://host:8080/v1
//	AGENTOS_REAL_MODEL_NAME=DeepSeek-V4-Flash-w8a8-mtp
//	AGENTOS_REAL_MODEL_KEY=optional-bearer (unset for keyless vLLM)
//
// Unset variables skip the test.
package e2e_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/model/provider"
)

func TestV11RealModelSmoke(t *testing.T) {
	baseURL := os.Getenv("AGENTOS_REAL_MODEL_URL")
	modelName := os.Getenv("AGENTOS_REAL_MODEL_NAME")
	if baseURL == "" || modelName == "" {
		t.Skip("AGENTOS_REAL_MODEL_URL and AGENTOS_REAL_MODEL_NAME are not set")
	}
	config := provider.Config{
		Name: "real", BaseURL: baseURL, TimeoutMs: 120000, MaxAttempts: 2,
		BreakerOpens: 5, BreakerCoolMs: 30000,
	}
	if key := os.Getenv("AGENTOS_REAL_MODEL_KEY"); key != "" {
		config.APIKey = key
	}
	executor := provider.NewExecutor(config, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := executor.Health(ctx); err != nil {
		t.Fatalf("health probe against the real endpoint: %v", err)
	}

	messages := []provider.Message{{Role: "user", Content: "Reply with exactly: agentos real model smoke ok"}}

	// Non-streaming: exact provider-reported usage and request id.
	complete, err := executor.Complete(ctx, provider.Invocation{ModelName: modelName, Messages: messages, MaxOutputTokens: 64})
	if err != nil {
		t.Fatalf("real completion: %v", err)
	}
	if strings.TrimSpace(complete.Content) == "" {
		t.Fatalf("empty completion: %+v", complete)
	}
	if complete.InputTokens <= 0 || complete.OutputTokens <= 0 {
		t.Fatalf("usage not reported: %+v", complete)
	}
	if complete.ProviderRequestID == "" {
		t.Fatalf("provider request id missing: %+v", complete)
	}
	t.Logf("real model (non-stream): usage=%d/%d finish=%s request=%s content=%q",
		complete.InputTokens, complete.OutputTokens, complete.FinishReason, complete.ProviderRequestID, truncate(complete.Content, 80))

	// Streaming: deltas delivered incrementally with exact final usage.
	var deltas []string
	streamed, err := executor.Stream(ctx, provider.Invocation{ModelName: modelName, Messages: messages, MaxOutputTokens: 64, Stream: true}, func(delta string) {
		deltas = append(deltas, delta)
	})
	if err != nil {
		t.Fatalf("real stream: %v", err)
	}
	if len(deltas) == 0 {
		t.Fatal("no deltas were delivered")
	}
	if streamed.InputTokens <= 0 || streamed.OutputTokens <= 0 {
		t.Fatalf("stream usage not reported: %+v", streamed)
	}
	if strings.Join(deltas, "") != streamed.Content {
		t.Fatalf("assembled content diverges from the delta stream")
	}
	t.Logf("real model (stream): deltas=%d usage=%d/%d finish=%s request=%s",
		len(deltas), streamed.InputTokens, streamed.OutputTokens, streamed.FinishReason, streamed.ProviderRequestID)
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

// TestV11RealAgentAgainstLiveModel runs the full v1.1 governed loop against
// a live self-hosted model: real inference flows through the fenced Model
// Gateway (policy, budget, ledger, audit), the agent writes its run summary
// to memory, and the task completes with the exact live usage settled once.
// Same environment gates as the smoke test plus AGENTOS_TEST_DATABASE_URL.
func TestV11RealAgentAgainstLiveModel(t *testing.T) {
	baseURL := os.Getenv("AGENTOS_REAL_MODEL_URL")
	modelName := os.Getenv("AGENTOS_REAL_MODEL_NAME")
	if baseURL == "" || modelName == "" {
		t.Skip("AGENTOS_REAL_MODEL_URL and AGENTOS_REAL_MODEL_NAME are not set")
	}
	if os.Getenv("AGENTOS_TEST_DATABASE_URL") == "" {
		t.Skip("AGENTOS_TEST_DATABASE_URL is not set")
	}
	e2eLiveModel.provider, e2eLiveModel.name, e2eLiveModel.url = "deepseek", modelName, baseURL
	e2eModelRef = "deepseek/" + modelName
	defer func() {
		e2eLiveModel.url = ""
		e2eModelRef = "fake/agent-model"
	}()

	env := newE2EEnv(t, "agentos_e2e_live", 0, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stack := startStack(t, env)
	worker := makeWorker(t, env, stack, "worker-live-1", 30*time.Second)

	publishVersion(ctx, t, env, stack.agentURL)
	task := submitTask(ctx, t, env, "live-real-1",
		"Answer in one sentence: why do production agent workloads need budget enforcement?")
	reconcile(ctx, t, env, poolsFor("worker-live-1"), false)
	go driveWorker(ctx, t, worker, "worker-live-1")

	finished := waitForTerminal(ctx, t, env, task.ID, 3*time.Minute)
	if finished.Phase != "SUCCEEDED" {
		attempts := queryInt(ctx, t, env, `SELECT count(*) FROM attempts a JOIN runs r ON r.id = a.run_id WHERE r.task_id = $1`, task.ID)
		t.Fatalf("live task phase = %s (attempts=%d), want SUCCEEDED", finished.Phase, attempts)
	}

	var calls int
	var usage int64
	var requestID string
	rows, err := env.pool.Query(ctx, `SELECT status, input_tokens + output_tokens, provider_request_id FROM model_calls
		WHERE tenant_id = $1 AND task_id = $2`, e2eTenant, task.ID)
	if err != nil {
		t.Fatalf("read live model calls: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var callUsage int64
		var id string
		if err := rows.Scan(&status, &callUsage, &id); err != nil {
			t.Fatalf("scan live model call: %v", err)
		}
		if status != "COMPLETED" {
			t.Fatalf("live model call status = %s", status)
		}
		calls++
		usage += callUsage
		if id != "" {
			requestID = id
		}
	}
	if calls == 0 || usage <= 0 || requestID == "" {
		t.Fatalf("live model ledger: calls=%d usage=%d requestID=%q", calls, usage, requestID)
	}
	settled := queryInt(ctx, t, env, `SELECT COALESCE(SUM(tokens),0) FROM task_budget_settlements WHERE task_id = $1`, task.ID)
	if settled != usage {
		t.Fatalf("settled tokens = %d, want exactly the live usage %d", settled, usage)
	}
	memories := queryInt(ctx, t, env, `SELECT count(*) FROM memory_records WHERE tenant_id = $1 AND namespace = 'runs'`, e2eTenant)
	if memories != 1 {
		t.Fatalf("memory records = %d, want 1", memories)
	}
	result := readResultArtifact(t, env, finished.ResultRef)
	output, _ := result["output"].(map[string]any)
	if output == nil || strings.TrimSpace(fmt.Sprint(output["answer"])) == "" {
		t.Fatalf("live result output = %#v", result["output"])
	}
	t.Logf("live-model governed loop: modelCalls=%d exactUsage=%d settled=%d answer=%q",
		calls, usage, settled, truncate(fmt.Sprint(output["answer"]), 100))
}
