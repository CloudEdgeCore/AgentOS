package provider

import (
	"context"
	"encoding/json"
)

// ToolDefinition describes an executable tool and its input schema.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolInvocationRequest contains the arguments for a tool call.
type ToolInvocationRequest struct {
	ToolName    string          `json:"toolName"`
	Arguments   json.RawMessage `json:"arguments"`
	ExecutionID string          `json:"executionId,omitempty"`
}

// ToolInvocationResult contains the tool's execution outcome and audit receipt.
type ToolInvocationResult struct {
	Output    json.RawMessage `json:"output"`
	ReceiptID string          `json:"receiptId,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// ToolProvider supplies capabilities and tools callable by agents.
type ToolProvider interface {
	Provider
	ListTools(ctx context.Context) ([]ToolDefinition, error)
	InvokeTool(ctx context.Context, req ToolInvocationRequest) (ToolInvocationResult, error)
}
