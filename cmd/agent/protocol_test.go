package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProtocolConfiguration(t *testing.T) {
	cfg := DefaultConfig()

	// 1. Verify default protocols
	if p, ok := cfg.LLM.Providers["openrouter"]; !ok || p.Protocol != "openai" {
		t.Fatalf("expected openrouter to have protocol 'openai', got %v", p.Protocol)
	}
	if p, ok := cfg.LLM.Providers["anthropic"]; !ok || p.Protocol != "anthropic" {
		t.Fatalf("expected anthropic to have protocol 'anthropic', got %v", p.Protocol)
	}

	// 2. Test YAML persistence and roundtrip with protocol
	tmpDir := t.TempDir()
	yamlFile := filepath.Join(tmpDir, "agent_proto.yaml")

	cfg.LLM.DefaultProvider = "anthropic"
	curr := cfg.CurrentProvider()
	curr.Protocol = "anthropic"
	curr.BaseURL = "https://custom-claude-gateway.internal/v1"
	cfg.LLM.Providers["anthropic"] = curr

	if err := SaveConfig(cfg, yamlFile); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	loadedCfg := DefaultConfig()
	if err := mergeYAMLFile(loadedCfg, yamlFile); err != nil {
		t.Fatalf("mergeYAMLFile failed: %v", err)
	}

	p := loadedCfg.LLM.Providers["anthropic"]
	if p.Protocol != "anthropic" {
		t.Errorf("expected loaded protocol to be 'anthropic', got %s", p.Protocol)
	}
	if p.BaseURL != "https://custom-claude-gateway.internal/v1" {
		t.Errorf("expected loaded base_url to match, got %s", p.BaseURL)
	}
}

func TestStreamLLMProtocolsMock(t *testing.T) {
	// 1. Mock OpenAI Compatible Server
	openAIMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("OpenAI: unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-openai-key" {
			t.Errorf("OpenAI: missing or invalid Authorization header: %s", r.Header.Get("Authorization"))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"Evaluating status.\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Kernel operational via OpenAI protocol.\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer openAIMock.Close()

	// 2. Mock Anthropic Server
	anthropicMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("Anthropic: unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-anthropic-key" {
			t.Errorf("Anthropic: missing or invalid x-api-key header: %s", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("Anthropic: missing or invalid anthropic-version: %s", r.Header.Get("anthropic-version"))
		}

		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["max_tokens"] == nil {
			t.Errorf("Anthropic: expected max_tokens in request body")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Kernel operational via Anthropic protocol.\"}}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer anthropicMock.Close()

	// Test OpenAI streaming
	openAICfg := DefaultConfig()
	openAICfg.LLM.DefaultProvider = "mock-openai"
	openAICfg.LLM.Providers["mock-openai"] = ProviderConfig{
		Model:      "mock-openai-model",
		APIKey:     "test-openai-key",
		BaseURL:    openAIMock.URL,
		Protocol:   "openai",
		TimeoutSec: 10,
	}

	tokensOpenAI, err := StreamLLM(openAICfg, "system", "user")
	if err != nil {
		t.Fatalf("StreamLLM OpenAI failed: %v", err)
	}
	if tokensOpenAI <= 0 {
		t.Errorf("expected positive token count, got %d", tokensOpenAI)
	}

	// Test Anthropic streaming
	anthropicCfg := DefaultConfig()
	anthropicCfg.LLM.DefaultProvider = "mock-anthropic"
	anthropicCfg.LLM.Providers["mock-anthropic"] = ProviderConfig{
		Model:      "claude-3-5-sonnet",
		APIKey:     "test-anthropic-key",
		BaseURL:    anthropicMock.URL,
		Protocol:   "anthropic",
		TimeoutSec: 10,
	}

	tokensAnthropic, err := StreamLLM(anthropicCfg, "system", "user")
	if err != nil {
		t.Fatalf("StreamLLM Anthropic failed: %v", err)
	}
	if tokensAnthropic <= 0 {
		t.Errorf("expected positive token count, got %d", tokensAnthropic)
	}
}

func TestUIConfigProtocolEndpoints(t *testing.T) {
	// Test POST /api/config with protocol
	fullCfgReq := UIFullConfigRequest{
		Environment:     "staging",
		DefaultProvider: "custom-anthropic",
		Model:           "claude-3-5-haiku",
		BaseURL:         "https://api.anthropic.com/v1",
		Protocol:        "anthropic",
		APIKey:          "sk-ant-test-key",
		TimeoutSec:      60,
		BudgetUSD:       3.00,
	}
	b, _ := json.Marshal(fullCfgReq)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleUIConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("handleUIConfig failed: status %d, body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("expected success true, got %v", resp["success"])
	}

	// Clean up generated agent.yaml if created in working dir
	_ = os.Remove("agent.yaml")
}
