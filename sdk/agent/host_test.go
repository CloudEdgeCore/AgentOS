package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type testRuntime struct {
	mu         sync.Mutex
	restored   RestoreRequest
	checkpoint Checkpoint
	block      bool
}

func (r *testRuntime) Run(ctx context.Context, request StartRequest, emit Emitter) (json.RawMessage, error) {
	if err := emit("agent.started", json.RawMessage(`{"goal":"`+request.Goal+`"}`)); err != nil {
		return nil, err
	}
	if r.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return json.RawMessage(`{"answer":"done"}`), nil
}

func (r *testRuntime) Checkpoint(context.Context, string) (Checkpoint, error) {
	return r.checkpoint, nil
}

func (r *testRuntime) Restore(_ context.Context, request RestoreRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restored = request
	return nil
}

func startFixture(id string) StartRequest {
	return StartRequest{
		ExecutionID: id, AgentVersionRef: "sample@1.0.0", Goal: "test",
		Input: json.RawMessage(`{"value":1}`),
		Capabilities: CapabilityGrant{
			Tools: []string{}, Models: []string{}, Memory: []string{}, Secrets: []string{},
		},
	}
}

func newTestClient(t *testing.T, runtime Runtime) (*Client, *httptest.Server) {
	t.Helper()
	host, err := NewHost(runtime, HostOptions{Adapter: "go-native", MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

func TestHostLifecycleIsIdempotentAndObservable(t *testing.T) {
	runtime := &testRuntime{checkpoint: Checkpoint{
		SchemaVersion: "sample/v1", State: json.RawMessage(`{"offset":1}`), CreatedAt: time.Now().UTC(),
	}}
	client, _ := newTestClient(t, runtime)
	ctx := context.Background()
	health, err := client.Health(ctx)
	if err != nil || health.Status != "SERVING" || health.Adapter != "go-native" {
		t.Fatalf("health=%+v err=%v", health, err)
	}
	started, err := client.Start(ctx, startFixture("exec-1"))
	if err != nil || started.ExecutionID != "exec-1" {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	replayed, err := client.Start(ctx, startFixture("exec-1"))
	if err != nil || !replayed.Replayed {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	result, err := client.WaitResult(ctx, "exec-1", time.Millisecond)
	if err != nil || result.Status != StatusSucceeded || string(result.Output) != `{"answer":"done"}` {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	events, err := client.Events(ctx, "exec-1", 0)
	if err != nil || len(events.Events) != 1 || events.Events[0].Sequence != 1 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	checkpoint, err := client.Checkpoint(ctx, "exec-1")
	if err != nil || checkpoint.Checkpoint.SchemaVersion != "sample/v1" {
		t.Fatalf("checkpoint=%+v err=%v", checkpoint, err)
	}
	restored, err := client.Restore(ctx, RestoreRequest{ExecutionID: "exec-2", Checkpoint: checkpoint.Checkpoint})
	if err != nil || !restored.Restored {
		t.Fatalf("restore=%+v err=%v", restored, err)
	}
}

func TestLegacyClientUsesDeprecatedAlphaRoute(t *testing.T) {
	host, err := NewHost(&testRuntime{}, HostOptions{Adapter: "legacy-compatible"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(host)
	defer server.Close()
	client, err := NewLegacyClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	health, err := client.Health(context.Background())
	if err != nil || len(health.ProtocolVersions) != 1 || health.ProtocolVersions[0] != LegacyProtocolVersion {
		t.Fatalf("legacy health=%+v err=%v", health, err)
	}
}

func TestHostRejectsConflictsImplicitCapabilitiesAndUnknownFields(t *testing.T) {
	client, server := newTestClient(t, &testRuntime{})
	request := startFixture("exec-conflict")
	if _, err := client.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Goal = "different"
	if _, err := client.Start(context.Background(), request); err == nil {
		t.Fatal("conflicting execution replay was accepted")
	}

	request = startFixture("exec-implicit")
	request.Capabilities.Secrets = nil
	if _, err := client.Start(context.Background(), request); err == nil {
		t.Fatal("implicit capability default was accepted")
	}

	reply, err := server.Client().Post(server.URL+"/v1alpha1/executions:start", "application/json",
		strings.NewReader(`{"executionId":"e","agentVersionRef":"a@1","goal":"g","input":{},"capabilities":{"tools":[],"models":[],"memory":[],"secrets":[]},"unknown":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	if reply.StatusCode != 400 {
		t.Fatalf("unknown field status=%d", reply.StatusCode)
	}
	duplicate, err := server.Client().Post(server.URL+"/v1alpha1/executions:start", "application/json",
		strings.NewReader(`{"executionId":"e","executionId":"other","agentVersionRef":"a@1","goal":"g","input":{},"capabilities":{"tools":[],"models":[],"memory":[],"secrets":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer duplicate.Body.Close()
	if duplicate.StatusCode != 400 {
		t.Fatalf("duplicate field status=%d", duplicate.StatusCode)
	}
	missing, err := server.Client().Post(server.URL+"/v1alpha1/executions:start", "application/json",
		strings.NewReader(`{"executionId":"missing","agentVersionRef":"a@1","goal":"g","capabilities":{"tools":[],"models":[],"memory":[],"secrets":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != 400 {
		t.Fatalf("missing required input status=%d", missing.StatusCode)
	}
}

func TestClientRejectsAmbiguousBaseURLs(t *testing.T) {
	for _, endpoint := range []string{"http://user@localhost", "http://localhost/runtime", "http://localhost?target=x", "http://localhost#fragment"} {
		if _, err := NewClient(endpoint, nil); err == nil {
			t.Fatalf("ambiguous endpoint %q was accepted", endpoint)
		}
	}
}

func TestHostBoundsRetainedExecutions(t *testing.T) {
	host, err := NewHost(&testRuntime{}, HostOptions{Adapter: "bounded", MaxConcurrent: 1, ExecutionLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(host)
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Start(context.Background(), startFixture("old")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.WaitResult(context.Background(), "old", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Start(context.Background(), startFixture("new")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Result(context.Background(), "old"); err == nil {
		t.Fatal("old terminal execution was retained beyond the configured bound")
	}
}

func TestHostStopCancelsExecution(t *testing.T) {
	client, _ := newTestClient(t, &testRuntime{block: true})
	if _, err := client.Start(context.Background(), startFixture("exec-stop")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Stop(context.Background(), "exec-stop"); err != nil {
		t.Fatal(err)
	}
	result, err := client.WaitResult(context.Background(), "exec-stop", time.Millisecond)
	if err != nil || result.Status != StatusCancelled {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestHostRejectsInvalidAdapterResult(t *testing.T) {
	client, _ := newTestClient(t, invalidRuntime{})
	if _, err := client.Start(context.Background(), startFixture("exec-invalid")); err != nil {
		t.Fatal(err)
	}
	result, err := client.WaitResult(context.Background(), "exec-invalid", time.Millisecond)
	if err != nil || result.Status != StatusFailed || result.ErrorCode != "INVALID_RESULT" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

type invalidRuntime struct{}

func (invalidRuntime) Run(context.Context, StartRequest, Emitter) (json.RawMessage, error) {
	return json.RawMessage(`not-json`), nil
}
func (invalidRuntime) Checkpoint(context.Context, string) (Checkpoint, error) {
	return Checkpoint{}, errors.New("unsupported")
}
func (invalidRuntime) Restore(context.Context, RestoreRequest) error {
	return errors.New("unsupported")
}
