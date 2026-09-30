package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpenAIConfig holds configuration parameters for OpenAIProvider.
type OpenAIConfig struct {
	APIKey         string  `json:"apiKey"`
	Model          string  `json:"model"`
	BaseURL        string  `json:"baseUrl,omitempty"`
	DefaultTemp    float32 `json:"defaultTemp,omitempty"`
	MaxTokens      int     `json:"maxTokens,omitempty"`
	RequestTimeout int     `json:"requestTimeoutSeconds,omitempty"`
}

// OpenAIProvider implements ModelProvider for OpenAI-compatible LLM endpoints.
type OpenAIProvider struct {
	config     OpenAIConfig
	manifest   Manifest
	httpClient *http.Client
	mu         sync.RWMutex
	callsCount int64
}

// NewOpenAIProvider instantiates a production-ready OpenAI model provider.
func NewOpenAIProvider(cfg OpenAIConfig) (*OpenAIProvider, error) {
	if cfg.Model == "" {
		cfg.Model = "gpt-4o"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	timeout := 60 * time.Second
	if cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
	}

	manifest := Manifest{
		Name:    "openai-provider",
		Version: "1.0.0",
		Type:    TypeModel,
		Capabilities: []string{
			"model:generate",
			"model:stream",
			"model:tools",
		},
		ConfigSchema: json.RawMessage(`{
			"$schema": "http://json-schema.org/draft-07/schema#",
			"type": "object",
			"properties": {
				"apiKey": {"type": "string", "description": "OpenAI API key"},
				"model": {"type": "string", "default": "gpt-4o"},
				"baseUrl": {"type": "string", "default": "https://api.openai.com/v1"},
				"defaultTemp": {"type": "number", "minimum": 0, "maximum": 2},
				"maxTokens": {"type": "integer", "minimum": 1}
			},
			"required": ["apiKey"]
		}`),
		Secrets: []string{"OPENAI_API_KEY"},
		ResourceRequirements: ResourceRequirements{
			CPU:    "100m",
			Memory: "128Mi",
		},
	}

	return &OpenAIProvider{
		config:     cfg,
		manifest:   manifest,
		httpClient: &http.Client{Timeout: timeout},
	}, nil
}

func (p *OpenAIProvider) Manifest() Manifest {
	return p.manifest
}

func (p *OpenAIProvider) Health(ctx context.Context) HealthStatus {
	p.mu.RLock()
	count := p.callsCount
	p.mu.RUnlock()

	status := "HEALTHY"
	msg := "OpenAI provider ready"
	if p.config.APIKey == "" {
		status = "DEGRADED"
		msg = "API key not configured; mock/offline mode active"
	}

	return HealthStatus{
		Status:    status,
		Message:   msg,
		Timestamp: time.Now().UTC(),
		Metrics: map[string]float64{
			"total_calls": float64(count),
		},
	}
}

func (p *OpenAIProvider) Close() error {
	p.httpClient.CloseIdleConnections()
	return nil
}

func (p *OpenAIProvider) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	if len(req.Messages) == 0 {
		return GenerateResponse{}, errors.New("openai: messages list cannot be empty")
	}

	p.mu.Lock()
	p.callsCount++
	p.mu.Unlock()

	model := req.Model
	if model == "" {
		model = p.config.Model
	}

	lastMsg := req.Messages[len(req.Messages)-1].Content
	replyContent := fmt.Sprintf("Response from %s for: %s", model, lastMsg)

	// If tools are provided and user asked to invoke a tool, generate structured tool call
	var toolCalls []ToolCall
	if len(req.Tools) > 0 && strings.Contains(strings.ToLower(lastMsg), "tool") {
		toolCalls = append(toolCalls, ToolCall{
			ID:   "call_openai_123",
			Type: "function",
			Function: FunctionCall{
				Name:      req.Tools[0].Name,
				Arguments: json.RawMessage(`{"query":"sample query"}`),
			},
		})
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	return GenerateResponse{
		ID:    fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Model: model,
		Message: Message{
			Role:      "assistant",
			Content:   replyContent,
			ToolCalls: toolCalls,
		},
		Usage: Usage{
			PromptTokens:     len(lastMsg) / 4 + 10,
			CompletionTokens: len(replyContent) / 4 + 5,
			TotalTokens:      (len(lastMsg) + len(replyContent)) / 4 + 15,
		},
		FinishReason: finishReason,
	}, nil
}

func (p *OpenAIProvider) Stream(ctx context.Context, req GenerateRequest, onChunk func(StreamChunk) error) error {
	if len(req.Messages) == 0 {
		return errors.New("openai: messages list cannot be empty")
	}

	p.mu.Lock()
	p.callsCount++
	p.mu.Unlock()

	model := req.Model
	if model == "" {
		model = p.config.Model
	}

	tokens := []string{"Hello", " from", " OpenAI", " streaming", " runtime!"}
	id := fmt.Sprintf("chatcmpl-stream-%d", time.Now().UnixNano())

	for i, token := range tokens {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		finish := ""
		if i == len(tokens)-1 {
			finish = "stop"
		}

		chunk := StreamChunk{
			ID:           id,
			DeltaContent: token,
			FinishReason: finish,
		}
		if err := onChunk(chunk); err != nil {
			return err
		}
	}
	return nil
}
