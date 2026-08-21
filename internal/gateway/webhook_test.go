package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/tool"
)

func TestWebhookExecutorPinsHTTPSDestinationAndBoundsProtocol(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("AgentOS-Secret-Handle") != "opaque" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("security headers were not injected")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || string(body["action"]) != `"search"` {
			t.Errorf("body=%s err=%v", body, err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	executor, err := NewWebhookExecutor(map[string]string{"search@1.0.0": server.URL}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), tool.ExecutionRequest{
		Descriptor: store.ToolDescriptor{Name: "search", Version: "1.0.0"}, Action: "search",
		Resource: "index", Args: json.RawMessage(`{"q":"agent"}`), Secret: "opaque",
	})
	if err != nil || string(result.Output) != `{"ok":true}` {
		t.Fatalf("result=%s err=%v", result.Output, err)
	}
}

func TestWebhookExecutorRejectsMutableOrInsecureDestinations(t *testing.T) {
	for name, endpoints := range map[string]map[string]string{
		"http":        {"search@1": "http://example.com/tool"},
		"credentials": {"search@1": "https://user@example.com/tool"},
		"query":       {"search@1": "https://example.com/tool?redirect=x"},
		"bad key":     {"search": "https://example.com/tool"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewWebhookExecutor(endpoints, nil); err == nil {
				t.Fatal("unsafe endpoint configuration was accepted")
			}
		})
	}
}
