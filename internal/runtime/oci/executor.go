// Package oci provides the OCI + gVisor (runsc) Runtime Provider worker. It
// implements the fenced Runtime Protocol loop from ADR-006 and delegates
// workload execution to a sandboxed Executor so the protocol logic stays
// testable without containerd or runsc installed.
package oci

import (
	"context"
	"io"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
)

const (
	// ProviderName identifies this provider in checkpoints and audit records.
	ProviderName = "oci-gvisor"
	// RuntimeABI is the provider-level Runtime ABI declared in checkpoint envelopes.
	RuntimeABI = "agentos.oci/v1"
	// CheckpointSchema is the logical checkpoint state schema version.
	CheckpointSchema = "agentos.oci-logical/v1"

	checkpointMediaType = "application/vnd.agentos.oci-checkpoint+json"
	resultMediaType     = "application/vnd.agentos.oci-result+json"
)

// ArtifactStore is the content-addressed store used for checkpoint state and
// results. The filesystem adapter is development-only; production must use an
// S3-compatible adapter with the same artifact:// identity.
type ArtifactStore interface {
	Put(context.Context, string, string, io.Reader) (store.ArtifactReference, error)
	Open(context.Context, string, store.ArtifactReference) (io.ReadCloser, error)
}

// ExecutionSpec is the provider-agnostic input to run one Attempt in a
// sandboxed container. All fields come from the fenced Assignment and from
// validated workload metadata; the Executor must never trust container-supplied
// values for its own resource or identity decisions.
type ExecutionSpec struct {
	TenantID          string
	AttemptID         string
	AgentVersionRef   string
	WorkloadSpecJSON  []byte
	ImageRef          string
	WorkspaceBytes    int64
	CPUQuotaMillis    int64
	MemoryLimitMiB    int64
	RuntimeClass      string
	RuntimePoolID     string
	RuntimeInstanceID string
}

// RunResult is what a provider reports after the workload terminates. Output
// bytes are not returned by design: container stdout must be spooled to the
// artifact store in a later slice and must never enter kernel messages
// unbounded.
type RunResult struct {
	ExitCode    int
	UsageMillis int64
	// FailureCode is an optional machine-readable classification. When empty,
	// the worker derives one from ExitCode.
	FailureCode string
}

// Execution is a prepared workload owned by the provider.
type Execution interface {
	// ID is the provider-side sandbox identifier (for example the containerd
	// container ID), used by Destroy.
	ID() string
	// Wait blocks until the workload terminates or ctx is cancelled. On
	// cancellation the provider must request termination of the workload.
	Wait(ctx context.Context) (RunResult, error)
}

// Executor prepares and destroys sandboxed executions. Implementations must be
// safe for concurrent use by multiple workers on the same RuntimeInstance.
type Executor interface {
	Prepare(ctx context.Context, spec ExecutionSpec) (Execution, error)
	Destroy(ctx context.Context, execution Execution) error
}

// RunscOption configures the containerd/runsc executor. It is declared here so
// the non-Linux stub can reference it; the concrete options and methods live in
// runsc_linux.go.
type RunscOption func(*ctrExecutor)

// ctrExecutor is the containerd CLI + runsc executor. The struct is declared
// here (platform-neutral fields) so the non-Linux stub can name the type; its
// constructor and methods are Linux-only and live in runsc_linux.go.
type ctrExecutor struct {
	ctrPath     string
	namespace   string
	runtime     string
	skipPull    bool
	pullTimeout time.Duration
	outputLimit int64
}

// WithNamespace sets the containerd namespace (default "agentos").
func WithNamespace(namespace string) RunscOption {
	return func(e *ctrExecutor) { e.namespace = namespace }
}

// WithRuntime sets the containerd runtime name (default "io.containerd.runsc.v1").
func WithRuntime(runtime string) RunscOption {
	return func(e *ctrExecutor) { e.runtime = runtime }
}

// WithSkipPull skips the image pull step (development images pre-loaded into
// containerd only).
func WithSkipPull() RunscOption {
	return func(e *ctrExecutor) { e.skipPull = true }
}

// ExecutionSpecLimits returns the resource limits encoded in an ExecutionSpec
// as (cpuMillis, memoryMiB, workspaceBytes); zero values mean "no explicit
// limit", which Admission must not allow for untrusted workloads.
func (s ExecutionSpec) Limits() (cpuMillis, memoryMiB, workspaceBytes int64) {
	return s.CPUQuotaMillis, s.MemoryLimitMiB, s.WorkspaceBytes
}
