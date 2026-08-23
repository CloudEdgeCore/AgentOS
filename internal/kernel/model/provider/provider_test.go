package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOpenAI is a scriptable OpenAI-compatible endpoint for the executor
// tests. It records the Authorization header and request bodies, and serves
// either a streaming SSE response or a plain JSON completion.
type fakeOpenAI struct {
	mu             sync.Mutex
	requests       int
	bodies         []map[string]any
	authorizations []string
	// script serves each request in order; the last entry repeats.
	script  []fakeResponse
	latency time.Duration
}

type fakeResponse struct {
	status     int
	retryAfter string
	stream     []string // raw SSE lines (already data: prefixed by helpers)
	body       string   // non-stream JSON body
	requestID  string
}

func (f *fakeOpenAI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	f.requests++
	var body map[string]any
	raw, _ := io.ReadAll(request.Body)
	_ = json.Unmarshal(raw, &body)
	f.bodies = append(f.bodies, body)
	f.authorizations = append(f.authorizations, request.Header.Get("Authorization"))
	index := f.requests - 1
	if index >= len(f.script) {
		index = len(f.script) - 1
	}
	response := f.script[index]
	latency := f.latency
	f.mu.Unlock()

	if latency > 0 {
		time.Sleep(latency)
	}
	if response.requestID != "" {
		writer.Header().Set("x-request-id", response.requestID)
	}
	if response.retryAfter != "" {
		writer.Header().Set("Retry-After", response.retryAfter)
	}
	if response.status != 0 && response.status/100 != 2 {
		http.Error(writer, response.body, response.status)
		return
	}
	if len(response.stream) > 0 {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		for _, line := range response.stream {
			fmt.Fprintf(writer, "data: %s\n\n", line)
		}
		fmt.Fprint(writer, "data: [DONE]\n\n")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	fmt.Fprint(writer, response.body)
}

func (f *fakeOpenAI) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeOpenAI) lastBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil
	}
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeOpenAI) authorization(index int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.authorizations) {
		return ""
	}
	return f.authorizations[index]
}

func streamChunks(deltas ...string) []string {
	lines := make([]string, 0, len(deltas)+2)
	for _, delta := range deltas {
		encoded, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-stream", "choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"content": delta}},
			},
		})
		lines = append(lines, string(encoded))
	}
	finish, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-stream", "choices": []map[string]any{
			{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
		},
		"usage": map[string]any{"prompt_tokens": 40, "completion_tokens": 20},
	})
	lines = append(lines, string(finish))
	return lines
}

func completionBody(content string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`, content)
}

func testExecutor(server *httptest.Server, breakerOpens int) *Executor {
	executor := NewExecutor(Config{
		Name: "fake", BaseURL: server.URL, APIKey: "secret-test-key",
		TimeoutMs: 5000, MaxAttempts: 3, BreakerOpens: breakerOpens, BreakerCoolMs: 60000,
		MaxConcurrent: 128, SupportsIdempotency: true,
	}, server.Client())
	executor.jitter = func(d time.Duration) time.Duration { return 0 }
	return executor
}

func TestExecutorCompleteNonStreamingExtractsUsageAndRequestID(t *testing.T) {
	server := &fakeOpenAI{script: []fakeResponse{{body: completionBody("hello there"), requestID: "req-42"}}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)

	result, err := executor.Complete(context.Background(), Invocation{ModelName: "fake-model", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if result.Content != "hello there" || result.FinishReason != "stop" {
		t.Fatalf("content=%q finish=%q", result.Content, result.FinishReason)
	}
	if result.InputTokens != 11 || result.OutputTokens != 7 {
		t.Fatalf("usage = %d/%d, want 11/7 (exact provider-reported tokens)", result.InputTokens, result.OutputTokens)
	}
	if result.ProviderRequestID != "req-42" {
		t.Fatalf("provider request id = %q, want header value req-42", result.ProviderRequestID)
	}
	// The outbound request carries the credential; it must never appear in
	// any result surface.
	if strings.Contains(fmt.Sprintf("%v", result), "secret-test-key") {
		t.Fatalf("result leaks the API key")
	}
	if got := server.authorization(0); got != "Bearer secret-test-key" {
		t.Fatalf("authorization = %q", got)
	}
}

func TestExecutorStreamDeliversDeltasAndExactUsage(t *testing.T) {
	server := &fakeOpenAI{script: []fakeResponse{{stream: streamChunks("Hel", "lo ", "world")}}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)

	var deltas []string
	result, err := executor.Stream(context.Background(), Invocation{ModelName: "m", Stream: true}, func(delta string) { deltas = append(deltas, delta) })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if strings.Join(deltas, "") != "Hello world" || result.Content != "Hello world" {
		t.Fatalf("deltas=%v content=%q", deltas, result.Content)
	}
	if result.InputTokens != 40 || result.OutputTokens != 20 {
		t.Fatalf("usage = %d/%d, want 40/20 from the include_usage chunk", result.InputTokens, result.OutputTokens)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("finish reason = %q", result.FinishReason)
	}
	// Streaming requests must negotiate usage reporting.
	body := server.lastBody()
	if body["stream"] != true {
		t.Fatalf("stream flag not sent: %v", body)
	}
	options, ok := body["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options.include_usage not sent: %v", body["stream_options"])
	}
}

func TestExecutorStreamAssemblesFragmentedToolCalls(t *testing.T) {
	first, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "tool_calls": []map[string]any{
				{"index": 0, "id": "call-1", "type": "function", "function": map[string]any{"name": "weather", "arguments": `{"ci`}},
			}},
		}},
	})
	second, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"tool_calls": []map[string]any{
				{"index": 0, "function": map[string]any{"arguments": `ty":"paris"}`}},
			}},
		}},
	})
	server := &fakeOpenAI{script: []fakeResponse{{stream: []string{string(first), string(second)}}}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)

	result, err := executor.Stream(context.Background(), Invocation{ModelName: "m", Stream: true}, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(result.ToolCalls))
	}
	call := result.ToolCalls[0]
	if call.ID != "call-1" || call.Name != "weather" || call.Arguments != `{"city":"paris"}` {
		t.Fatalf("assembled tool call = %+v", call)
	}
}

func TestExecutorMetersToolOnlyStreamAsDelivered(t *testing.T) {
	chunk, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []map[string]any{
						{"index": 0, "id": "call-1", "function": map[string]any{
							"name": "expensive_tool", "arguments": `{"value":42}`,
						}},
					},
				},
			},
		},
	})
	server := &fakeOpenAI{script: []fakeResponse{{stream: []string{string(chunk)}}}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)
	var generated strings.Builder
	result, err := executor.StreamObserved(context.Background(), Invocation{ModelName: "m", Stream: true}, StreamObserver{
		OnGenerated: func(delta string) { generated.WriteString(delta) },
	})
	if err != nil {
		t.Fatalf("tool-only stream: %v", err)
	}
	if generated.String() != `expensive_tool{"value":42}` || len(result.ToolCalls) != 1 {
		t.Fatalf("generated=%q calls=%+v", generated.String(), result.ToolCalls)
	}
}

func TestExecutorRetriesServerErrorsThenSucceeds(t *testing.T) {
	server := &fakeOpenAI{script: []fakeResponse{
		{status: http.StatusInternalServerError, body: "boom"},
		{status: http.StatusTooManyRequests, body: "slow down", retryAfter: "0"},
		{body: completionBody("recovered")},
	}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)

	result, err := executor.Complete(context.Background(), Invocation{ModelName: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil || result.Content != "recovered" {
		t.Fatalf("bounded retry did not recover: result=%+v err=%v", result, err)
	}
	if count := server.requestCount(); count != 3 {
		t.Fatalf("requests = %d, want 3 (initial + two retries)", count)
	}
}

func TestExecutorDoesNotRetryAmbiguousOutcomeWithoutIdempotency(t *testing.T) {
	server := &fakeOpenAI{script: []fakeResponse{
		{status: http.StatusInternalServerError, body: "possibly committed"},
		{body: completionBody("duplicate")},
	}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := NewExecutor(Config{Name: "unsafe", BaseURL: httpServer.URL, MaxAttempts: 3,
		TimeoutMs: 1000, MaxConcurrent: 8}, httpServer.Client())
	executor.jitter = func(time.Duration) time.Duration { return 0 }
	_, err := executor.Complete(context.Background(), Invocation{ModelName: "m", IdempotencyKey: "logical-call"})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("ambiguous error = %v", err)
	}
	if count := server.requestCount(); count != 1 {
		t.Fatalf("ambiguous call requests=%d, want 1", count)
	}
}

func TestExecutorToolDeltaMakesAbortedStreamNonRetryable(t *testing.T) {
	var requests atomic.Int64
	broken := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"name\":\"charge\",\"arguments\":\"{\"}}]}}]}\n\n")
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer broken.Close()
	executor := testExecutor(broken, 50)
	_, err := executor.Stream(context.Background(), Invocation{ModelName: "m"}, nil)
	if !errors.Is(err, ErrStreamAborted) {
		t.Fatalf("tool stream error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("tool stream retried %d times", requests.Load())
	}
}

func TestExecutorDoesNotRetryClientErrors(t *testing.T) {
	server := &fakeOpenAI{script: []fakeResponse{{status: http.StatusUnauthorized, body: "bad key"}}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)

	_, err := executor.Complete(context.Background(), Invocation{ModelName: "m"})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected provider rejection, got %v", err)
	}
	if count := server.requestCount(); count != 1 {
		t.Fatalf("requests = %d, want 1 (no retry on 4xx)", count)
	}
}

func TestExecutorSanitizesCredentialsAndURLInErrors(t *testing.T) {
	server := &fakeOpenAI{script: []fakeResponse{{status: http.StatusBadGateway, body: "upstream https://api.fake.internal/v1 failed with key secret-test-key"}}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)

	_, err := executor.Complete(context.Background(), Invocation{ModelName: "m"})
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), "secret-test-key") {
		t.Fatalf("error leaks the API key: %s", err)
	}
	if strings.Contains(err.Error(), httpServer.URL) {
		t.Fatalf("error leaks the endpoint URL: %s", err)
	}
}

func TestExecutorExhaustsRetryBudget(t *testing.T) {
	server := &fakeOpenAI{script: []fakeResponse{{status: http.StatusServiceUnavailable, body: "down"}}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 50)

	_, err := executor.Complete(context.Background(), Invocation{ModelName: "m"})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("expected unavailable error, got %v", err)
	}
	if count := server.requestCount(); count != 3 {
		t.Fatalf("requests = %d, want 3 (max attempts)", count)
	}
}

func TestExecutorCircuitBreakerOpensAndFailsFast(t *testing.T) {
	server := &fakeOpenAI{script: []fakeResponse{{status: http.StatusServiceUnavailable, body: "down"}}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 2)

	if _, err := executor.Complete(context.Background(), Invocation{ModelName: "m"}); err == nil {
		t.Fatal("expected failure")
	}
	countAfterTrips := server.requestCount()
	if countAfterTrips != 3 { // three attempts open the breaker (opens=2)
		t.Fatalf("requests = %d, want 3", countAfterTrips)
	}
	_, err := executor.Complete(context.Background(), Invocation{ModelName: "m"})
	if err == nil || !strings.Contains(err.Error(), "circuit breaker open") {
		t.Fatalf("expected fast-fail with open breaker, got %v", err)
	}
	if count := server.requestCount(); count != countAfterTrips {
		t.Fatalf("breaker did not fail fast: %d requests after opening", count-countAfterTrips)
	}
}

func TestExecutorStreamAbortAfterDeliveryIsNotRetried(t *testing.T) {
	// The stream delivers one delta, then the connection dies mid-body.
	broken := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"}}]}\n\n")
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		// No [DONE]: the executor must treat this as an aborted stream.
		panic(http.ErrAbortHandler)
	}))
	defer broken.Close()
	executor := testExecutor(broken, 50)

	var deltas []string
	_, err := executor.Stream(context.Background(), Invocation{ModelName: "m", Stream: true}, func(delta string) { deltas = append(deltas, delta) })
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("expected stream abort, got %v", err)
	}
	if len(deltas) != 1 {
		t.Fatalf("delivered deltas = %d, want 1", len(deltas))
	}
}

func TestExecutorHonorsContextCancellation(t *testing.T) {
	server := &fakeOpenAI{script: []fakeResponse{{body: completionBody("late")}}, latency: 2 * time.Second}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := executor.Complete(ctx, Invocation{ModelName: "m"})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("cancellation not honored: err=%v elapsed=%v", err, time.Since(start))
	}
}

func TestExecutorHealth(t *testing.T) {
	server := &fakeOpenAI{script: []fakeResponse{{body: `{"data":[]}`}}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)
	if err := executor.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	dead := NewExecutor(Config{Name: "dead", BaseURL: "http://127.0.0.1:1", TimeoutMs: 500, MaxAttempts: 1}, nil)
	if err := dead.Health(context.Background()); err == nil {
		t.Fatal("dead endpoint must not report healthy")
	}
}

// TestExecutorConcurrentInvocations verifies 100 concurrent calls against one
// endpoint produce 100 distinct correct results with no cross-talk (run with
// -race in CI).
func TestExecutorConcurrentInvocations(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		raw, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(raw, &body)
		prompt := ""
		if len(body.Messages) > 0 {
			prompt = body.Messages[len(body.Messages)-1].Content
		}
		fmt.Fprintf(writer, `{"id":"r","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`, "echo:"+prompt)
	}))
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)

	const concurrency = 100
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := executor.Complete(context.Background(), Invocation{
				ModelName: "m", Messages: []Message{{Role: "user", Content: fmt.Sprintf("msg-%d", i)}},
			})
			if err != nil {
				errs <- err
				return
			}
			if want := fmt.Sprintf("echo:msg-%d", i); result.Content != want {
				errs <- fmt.Errorf("cross-talk: got %q want %q", result.Content, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestExecutorGatewayOverheadP95 proves the Phase 1.2 performance acceptance:
// against a loopback endpoint (inference excluded by definition), the
// executor's added P95 latency stays under 100ms.
func TestExecutorGatewayOverheadP95(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, completionBody("ok"))
	}))
	defer httpServer.Close()
	executor := testExecutor(httpServer, 5)

	const calls = 200
	latencies := make([]time.Duration, 0, calls)
	for i := 0; i < calls; i++ {
		start := time.Now()
		if _, err := executor.Complete(context.Background(), Invocation{ModelName: "m", Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		latencies = append(latencies, time.Since(start))
	}
	sortDurations(latencies)
	p95 := latencies[(calls*95)/100]
	if p95 >= 100*time.Millisecond {
		t.Fatalf("gateway P95 overhead = %v, want < 100ms (inference excluded)", p95)
	}
	t.Logf("P95 gateway overhead over %d calls: %v", calls, p95)
}

func sortDurations(values []time.Duration) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func TestRegistryFailsClosedWithoutProvider(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Resolve("nowhere"); err == nil {
		t.Fatal("empty registry must fail closed")
	}
	if err := registry.Register(Config{Name: "ok", BaseURL: "https://example.com/v1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.Resolve("ok"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if names := registry.Names(); len(names) != 1 || names[0] != "ok" {
		t.Fatalf("names = %v", names)
	}
}

func TestLoadRegistryFile(t *testing.T) {
	t.Setenv("PROVIDER_TEST_KEY", "file-key")
	path := filepath.Join(t.TempDir(), "providers.json")
	document := `{"providers":[{"name":"qwen","baseUrl":"https://dashscope.example/v1","apiKeyEnv":"PROVIDER_TEST_KEY","maxAttempts":4}]}`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	registry, err := LoadRegistryFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	executor, err := registry.Resolve("qwen")
	if err != nil || executor.Name() != "qwen" {
		t.Fatalf("resolve: %v %v", executor, err)
	}
	// Keys must not be embeddable in the file.
	embedded := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(embedded, []byte(`{"providers":[{"name":"x","baseUrl":"https://x.example","apiKey":"leak"}]}`), 0o600)
	if _, err := LoadRegistryFile(embedded); err == nil {
		t.Fatal("embedded apiKey must be rejected")
	}
	if _, err := LoadRegistryFile(""); err != nil {
		t.Fatalf("empty path must yield an empty registry: %v", err)
	}
}

func TestExecutorResolvesAPIKeyFromEnvironment(t *testing.T) {
	t.Setenv("PROVIDER_ENV_KEY", "env-resolved-key")
	server := &fakeOpenAI{script: []fakeResponse{{body: completionBody("ok")}}}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	executor := NewExecutor(Config{Name: "env", BaseURL: httpServer.URL, APIKeyEnv: "PROVIDER_ENV_KEY", TimeoutMs: 2000, MaxAttempts: 1}, httpServer.Client())
	if _, err := executor.Complete(context.Background(), Invocation{ModelName: "m"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := server.authorization(0); got != "Bearer env-resolved-key" {
		t.Fatalf("authorization = %q, want env-resolved key", got)
	}
}

func TestExecutorTimeoutFailsFast(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprint(writer, completionBody("late"))
	}))
	defer slow.Close()
	executor := NewExecutor(Config{Name: "slow", BaseURL: slow.URL, TimeoutMs: 200, MaxAttempts: 2}, slow.Client())
	executor.jitter = func(d time.Duration) time.Duration { return 0 }
	start := time.Now()
	_, err := executor.Complete(context.Background(), Invocation{ModelName: "m"})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("expected timeout-classified error, got %v", err)
	}
	// Two attempts x 200ms timeout plus slack — well under the 2s handler.
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("timeout did not fail fast: %v", elapsed)
	}
}

func TestEncodeInvocationHonorsCapabilityProfile(t *testing.T) {
	disabled := false
	body, err := encodeInvocation(Invocation{ModelName: "reasoner", Stream: true, MaxOutputTokens: 64}, CapabilityProfile{
		StreamUsage: &disabled, MaxTokensField: "max_completion_tokens",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	text := string(body)
	if strings.Contains(text, "stream_options") || strings.Contains(text, `"max_tokens"`) || !strings.Contains(text, `"max_completion_tokens":64`) {
		t.Fatalf("capability profile not honored: %s", text)
	}
	if _, err := encodeInvocation(Invocation{ModelName: "no-tools", Tools: []ToolDefinition{{Name: "x"}}}, CapabilityProfile{ToolCalling: &disabled}); err == nil {
		t.Fatal("tool call was accepted by a provider that disables it")
	}
}

// P0-01: tool declarations must cross the wire in the OpenAI-compatible
// function-tool shape — {"type":"function","function":{name,description,
// parameters}} — because strict providers (vLLM, Qwen, DeepSeek, GLM, OpenAI)
// reject or ignore a bare flat object in "tools". The internal
// ToolDefinition stays flat; the conversion happens only here.
func TestEncodeInvocationUsesFunctionToolWireFormat(t *testing.T) {
	body, err := encodeInvocation(Invocation{
		ModelName: "m",
		Tools: []ToolDefinition{{
			Name:        "weather.lookup",
			Description: "look up weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var document struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if len(document.Tools) != 1 {
		t.Fatalf("tools = %d entries, want exactly one", len(document.Tools))
	}
	tool := document.Tools[0]
	if tool.Type != "function" {
		t.Fatalf("tool type = %q, want \"function\"", tool.Type)
	}
	if tool.Function.Name != "weather.lookup" || tool.Function.Description != "look up weather" {
		t.Fatalf("function envelope lost name/description: %+v", tool.Function)
	}
	if !strings.Contains(string(tool.Function.Parameters), `"city"`) {
		t.Fatalf("parameters schema not carried verbatim: %s", tool.Function.Parameters)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	encodedTool := raw["tools"]
	var toolsArray []map[string]json.RawMessage
	if err := json.Unmarshal(encodedTool, &toolsArray); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"type", "function"} {
		if _, ok := toolsArray[0][key]; !ok {
			t.Fatalf("wire tool missing %q field; strict OpenAI-compatible providers would reject: %s", key, encodedTool)
		}
	}
	for _, forbidden := range []string{"name", "description", "parameters"} {
		if _, ok := toolsArray[0][forbidden]; ok {
			t.Fatalf("flat field %q leaked to the top level of the wire tool: %s", forbidden, encodedTool)
		}
	}
}

var _ atomic.Int64
