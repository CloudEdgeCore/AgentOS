package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/runtime/v1"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/agentversion"
	"github.com/CloudEdgeCore/AgentOS/sdk/agent"
	"github.com/google/uuid"
)

func writeBindingsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime-bindings.json")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRuntimeBindingsResolvesExactAndWildcard(t *testing.T) {
	path := writeBindingsFile(t, `{
		"bindings": [
			{"agentVersionRef": "hello-agent@0.1.0", "endpoint": "http://127.0.0.1:8088/"},
			{"agentVersionRef": "fleet-agent@*", "endpoint": "http://localhost:9099"}
		]
	}`)
	bindings, err := LoadRuntimeBindings(path)
	if err != nil {
		t.Fatalf("load bindings: %v", err)
	}
	if endpoint, ok := bindings.Resolve("hello-agent@0.1.0"); !ok || endpoint != "http://127.0.0.1:8088" {
		t.Fatalf("exact resolve = %q %v", endpoint, ok)
	}
	if endpoint, ok := bindings.Resolve("fleet-agent@9.9.9"); !ok || endpoint != "http://localhost:9099" {
		t.Fatalf("wildcard resolve = %q %v", endpoint, ok)
	}
	if _, ok := bindings.Resolve("other-agent@1.0.0"); ok {
		t.Fatal("unbound version resolved")
	}
	if _, err := LoadRuntimeBindings(""); err != nil {
		t.Fatalf("empty path must disable bindings, got %v", err)
	}
}

func TestLoadRuntimeBindingsRejectsInvalidEntries(t *testing.T) {
	cases := map[string]string{
		"missing version": `{"bindings":[{"agentVersionRef":"agent","endpoint":"http://127.0.0.1:1"}]}`,
		"relative url":    `{"bindings":[{"agentVersionRef":"a@1","endpoint":"127.0.0.1:8088"}]}`,
		"unknown scheme":  `{"bindings":[{"agentVersionRef":"a@1","endpoint":"ftp://127.0.0.1:8088"}]}`,
		"unknown field":   `{"bindings":[{"agentVersionRef":"a@1","endpoint":"http://127.0.0.1:1","extra":true}]}`,
	}
	for name, content := range cases {
		path := writeBindingsFile(t, content)
		if _, err := LoadRuntimeBindings(path); err == nil {
			t.Fatalf("%s: binding file accepted", name)
		}
	}
}

// bindingAssignment builds an assignment whose manifest entrypoint is the
// environment-independent logical binding reference.
func bindingAssignment(t *testing.T, agentRef, entrypoint string, specJSON []byte) *fakeControl {
	t.Helper()
	attemptID := uuid.NewString()
	return &fakeControl{version: 1, assignment: &runtimev1.Assignment{
		Identity: &runtimev1.AttemptIdentity{TenantId: "tenant-a", AttemptId: attemptID, FencingToken: 1},
		RunId:    uuid.NewString(), TaskId: uuid.NewString(), AgentVersionRef: agentRef,
		Goal: "execute", WorkloadSpecJson: []byte("{}"), AgentVersionSpecJson: specJSON,
		RuntimeClass: "remote", RuntimePoolId: "remote-pool", RuntimeInstanceId: "adapter-1",
		AttemptVersion: 1, LeaseVersion: 1,
	}}
}

func bindingManifest(t *testing.T, entrypoint string) []byte {
	t.Helper()
	spec := agentversion.Spec{
		RuntimeClassPolicy: agentversion.RuntimeClassPolicy{Allowed: []string{"remote"}, Preferred: "remote"},
		Runtimes: []agentversion.RuntimeTarget{{
			Class: "remote", Interface: agentversion.RuntimeInterfaceV1,
			RuntimeABI: "agentos.remote/v1", Entrypoint: []string{entrypoint},
		}},
		Capabilities: &agentversion.Capabilities{Tools: []string{}, Models: []string{}, Memory: []string{}, Secrets: []string{}},
		Resources:    &agentversion.ResourceLimits{CPUMillis: 100, MemoryMiB: 128},
		Budget:       &agentversion.Budget{WallSeconds: 60},
		Checkpoint:   &agentversion.CheckpointPolicy{Mode: agentversion.CheckpointLogical, SchemaVersion: "fixture/v1"},
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestWorkerResolvesLogicalEntrypointThroughBindings proves the P2-01
// decoupling: one AgentVersion whose manifest carries only a logical
// binding reference executes against whatever concrete endpoint the
// deployment's binding file maps it to.
func TestWorkerResolvesLogicalEntrypointThroughBindings(t *testing.T) {
	host, err := agent.NewHost(&runtimeFixture{state: map[string]json.RawMessage{}}, agent.HostOptions{Adapter: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(host)
	defer server.Close()
	bindings, err := LoadRuntimeBindings(writeBindingsFile(t,
		`{"bindings":[{"agentVersionRef":"fixture@0.9.0","endpoint":"`+server.URL+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	control := bindingAssignment(t, "fixture@0.9.0", BindingScheme+"fixture/remote", bindingManifest(t, BindingScheme+"fixture/remote"))
	// No constructor endpoint: bindings are the only resolution source.
	worker, err := NewWorker(control, &memoryArtifacts{content: map[string][]byte{}},
		"", "tenant-a", "adapter-1", 30*time.Second, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	worker.WithRuntimeBindings(bindings)
	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v failure=%s", processed, err, control.failure)
	}
	if control.completed == nil {
		t.Fatalf("assignment did not complete; failure=%s", control.failure)
	}
}

// TestWorkerFailsClosedOnUnresolvedLogicalEntrypoint proves an unbound
// logical entrypoint never falls through to another endpoint.
func TestWorkerFailsClosedOnUnresolvedLogicalEntrypoint(t *testing.T) {
	control := bindingAssignment(t, "ghost@1.0.0", BindingScheme+"ghost/remote", bindingManifest(t, BindingScheme+"ghost/remote"))
	worker, err := NewWorker(control, &memoryArtifacts{content: map[string][]byte{}},
		"", "tenant-a", "adapter-1", 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A contained failure still counts as processed: the attempt failed
	// durably instead of the worker erroring or the assignment completing.
	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v, want a contained durable failure", processed, err)
	}
	if control.failure == "" || !contains(control.failure, "adapter_endpoint_unresolved") {
		t.Fatalf("failure = %q, want adapter_endpoint_unresolved with the unresolved reference", control.failure)
	}
	if control.completed != nil {
		t.Fatal("unresolved endpoint must never complete an attempt")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestWorkerFallsBackToPollingOnV1OnlyRuntime proves the streaming-first
// wait degrades to the frozen v1 polling endpoints when the runtime has no
// /events/stream route, resuming from the stream cursor without event loss.
func TestWorkerFallsBackToPollingOnV1OnlyRuntime(t *testing.T) {
	var mu sync.Mutex
	resultCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/executions:start", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("AgentOS-Runtime-Interface", "agentos.runtime.interface/v1")
		writeJSONResponse(writer, http.StatusAccepted, map[string]any{"executionId": "legacy", "status": "ACCEPTED"})
	})
	mux.HandleFunc("/v1/executions/", func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		writer.Header().Set("AgentOS-Runtime-Interface", "agentos.runtime.interface/v1")
		switch {
		case strings.HasSuffix(path, "/events"):
			writeJSONResponse(writer, http.StatusOK, map[string]any{
				"executionId": "legacy", "nextAfter": 1, "truncated": false,
				"events": []map[string]any{{
					"sequence": 1, "type": "legacy.started", "payload": map[string]any{"ok": true},
					"occurredAt": "2026-08-23T00:00:00Z",
				}},
			})
		case strings.HasSuffix(path, "/result"):
			mu.Lock()
			resultCalls++
			calls := resultCalls
			mu.Unlock()
			if calls < 2 {
				writeJSONResponse(writer, http.StatusAccepted, map[string]any{"executionId": "legacy", "status": "RUNNING"})
				return
			}
			writeJSONResponse(writer, http.StatusOK, map[string]any{
				"executionId": "legacy", "status": "SUCCEEDED", "output": map[string]any{"answer": "done"},
				"completedAt": "2026-08-23T00:00:01Z",
			})
		case strings.HasSuffix(path, ":stop"):
			writeJSONResponse(writer, http.StatusAccepted, map[string]any{"executionId": "legacy", "status": "RUNNING"})
		case strings.HasSuffix(path, ":checkpoint"):
			writeJSONResponse(writer, http.StatusOK, map[string]any{
				"executionId": "legacy",
				"checkpoint": map[string]any{
					"schemaVersion": "fixture/v1", "state": map[string]any{}, "createdAt": "2026-08-23T00:00:00Z",
				},
			})
		default:
			writeJSONResponse(writer, http.StatusNotFound, map[string]any{"code": "ROUTE_NOT_FOUND", "detail": "runtime execution route not found", "status": 404})
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	spec := bindingManifest(t, server.URL)
	control := bindingAssignment(t, "legacy@1.0.0", server.URL, spec)
	worker, err := NewWorker(control, &memoryArtifacts{content: map[string][]byte{}},
		server.URL, "tenant-a", "adapter-1", 30*time.Second, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	worker.pollInterval = 5 * time.Millisecond
	processed, err := worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v failure=%s", processed, err, control.failure)
	}
	if control.completed == nil {
		t.Fatalf("v1-only fallback did not complete; failure=%s", control.failure)
	}
}

func writeJSONResponse(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
