package provider

import (
	"context"
	"encoding/json"
)

// Message represents a prompt turn in chat completions.
type Message struct {
	Role    string          `json:"role"` // system, user, assistant, tool
	Content string          `json:"content"`
	Name    string          `json:"name,omitempty"`
	ToolCalls []ToolCall    `json:"toolCalls,omitempty"`
}

// ToolCall represents an assistant-requested tool invocation.
type ToolCall struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"` // function
	Function  FunctionCall    `json:"function"`
}

// FunctionCall describes the function and JSON arguments.
type FunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// GenerateRequest defines inputs to a language model invocation.
type GenerateRequest struct {
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages"`
	Temperature float32         `json:"temperature,omitempty"`
	MaxTokens   int             `json:"maxTokens,omitempty"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
}

// Usage reports token consumption for budget accounting.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
}

// GenerateResponse captures the completion result.
type GenerateResponse struct {
	ID        string    `json:"id"`
	Model     string    `json:"model"`
	Message   Message   `json:"message"`
	Usage     Usage     `json:"usage"`
	FinishReason string `json:"finishReason"` // stop, length, tool_calls
}

// StreamChunk conveys one delta in streaming model execution.
type StreamChunk struct {
	ID           string   `json:"id"`
	DeltaContent string   `json:"deltaContent,omitempty"`
	ToolCalls    []ToolCall `json:"toolCalls,omitempty"`
	FinishReason string   `json:"finishReason,omitempty"`
}

// ModelProvider provides text generation and streaming model capabilities.
type ModelProvider interface {
	Provider
	Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
	Stream(ctx context.Context, req GenerateRequest, onChunk func(StreamChunk) error) error
}
