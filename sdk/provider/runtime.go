package provider

import (
	"context"
	"encoding/json"
	"time"
)

// RuntimeStartRequest specifies the container, binary, or WASM component to launch.
type RuntimeStartRequest struct {
	ExecutionID      string            `json:"executionId"`
	ImageOrArtifact  string            `json:"imageOrArtifact"`
	Environment      map[string]string `json:"environment,omitempty"`
	ResourceLimits   ResourceRequirements `json:"resourceLimits,omitempty"`
	Entrypoint       []string          `json:"entrypoint,omitempty"`
}

// RuntimeStartResponse returns launch metadata.
type RuntimeStartResponse struct {
	ExecutionID string    `json:"executionId"`
	ContainerID string    `json:"containerId,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
}

// RuntimeStatusResponse returns current execution health.
type RuntimeStatusResponse struct {
	ExecutionID string    `json:"executionId"`
	State       string    `json:"state"` // RUNNING, STOPPED, FAILED
	ExitCode    int       `json:"exitCode,omitempty"`
	ResourceUsage map[string]float64 `json:"resourceUsage,omitempty"`
}

// RuntimeProvider launches and supervises isolated agent execution sandboxes.
type RuntimeProvider interface {
	Provider
	StartInstance(ctx context.Context, req RuntimeStartRequest) (RuntimeStartResponse, error)
	StopInstance(ctx context.Context, executionID string, gracePeriod time.Duration) error
	InspectInstance(ctx context.Context, executionID string) (RuntimeStatusResponse, error)
	StreamLogs(ctx context.Context, executionID string, onLog func(entry string) error) error
	CaptureSnapshot(ctx context.Context, executionID string) (json.RawMessage, error)
	RestoreSnapshot(ctx context.Context, executionID string, state json.RawMessage) error
}
