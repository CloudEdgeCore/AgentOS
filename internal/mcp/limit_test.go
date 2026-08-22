package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// blockingHandler parks every call until released, so the concurrency cap
// can be exercised deterministically.
type blockingHandler struct {
	blocked  bool
	entered  chan struct{}
	released chan struct{}
}

func (h *blockingHandler) ListTools(context.Context, json.RawMessage) (any, *Error) {
	return []any{}, nil
}

func (h *blockingHandler) CallTool(_ context.Context, _ json.RawMessage) (any, *Error) {
	if h.blocked {
		h.entered <- struct{}{}
		<-h.released
	}
	return map[string]any{"content": "ok"}, nil
}

func mcpRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	return request
}

func TestServerCapsConcurrentCalls(t *testing.T) {
	handler := &blockingHandler{blocked: true, entered: make(chan struct{}, 16), released: make(chan struct{})}
	server := NewServerWithConcurrencyLimit("test", "v1", handler, 2)

	// Two calls occupy the two slots.
	first := httptest.NewRecorder()
	second := httptest.NewRecorder()
	go server.ServeHTTP(first, mcpRequest(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`))
	go server.ServeHTTP(second, mcpRequest(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`))
	<-handler.entered
	<-handler.entered

	// The third call is rejected with 503 while the slots are full.
	third := httptest.NewRecorder()
	server.ServeHTTP(third, mcpRequest(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{}}`))
	if third.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-cap status = %d, want 503", third.Code)
	}

	// Releasing a slot lets the next call through.
	close(handler.released)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && server.active.Load() != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	fourth := httptest.NewRecorder()
	server.ServeHTTP(fourth, mcpRequest(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{}}`))
	if fourth.Code == http.StatusServiceUnavailable {
		t.Fatalf("call after slot release was still rejected")
	}
}
