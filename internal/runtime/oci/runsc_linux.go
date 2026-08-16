//go:build linux

package oci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ctrExecutor is the v0.1 engineering baseline for the OCI + gVisor provider.
// It drives the containerd CLI (ctr) with the runsc runtime; the full hardening
// surface (user namespaces, read-only rootfs, capability drop, seccomp,
// cgroups, NetworkPolicy, egress proxy) is enforced by Admission and by the
// runsc runtime itself — see docs/runtime-provider-oci-gvisor.md. The exact
// ctr subcommand surface must be re-validated against the pinned containerd
// version on the Linux integration CI before this provider is trusted.

// NewRunscExecutor constructs the containerd/runsc executor after verifying
// that the ctr CLI is available. It must run on Linux with containerd and
// runsc installed; the worker never falls back to an unsandboxed runtime.
func NewRunscExecutor(options ...RunscOption) (Executor, error) {
	path, err := exec.LookPath("ctr")
	if err != nil {
		return nil, fmt.Errorf("containerd CLI (ctr) is required by the OCI/gVisor provider: %w", err)
	}
	executor := &ctrExecutor{
		ctrPath: path, namespace: "agentos", runtime: "io.containerd.runsc.v1",
		pullTimeout: 10 * time.Minute, outputLimit: 1 << 20,
	}
	for _, option := range options {
		option(executor)
	}
	return executor, nil
}

func (e *ctrExecutor) Prepare(ctx context.Context, spec ExecutionSpec) (Execution, error) {
	if strings.TrimSpace(spec.ImageRef) == "" {
		return nil, fmt.Errorf("OCI image reference is required")
	}
	if strings.TrimSpace(spec.AttemptID) == "" {
		return nil, fmt.Errorf("attempt ID is required")
	}
	if strings.ContainsAny(spec.AttemptID, " \t\n\"'") {
		return nil, fmt.Errorf("attempt ID is not a safe container identifier")
	}
	if !e.skipPull {
		pullCtx, cancel := context.WithTimeout(ctx, e.pullTimeout)
		defer cancel()
		if err := e.run(pullCtx, "images", "pull", spec.ImageRef); err != nil {
			return nil, fmt.Errorf("pull workload image %s: %w", spec.ImageRef, err)
		}
	}

	containerID := "agentos-" + spec.AttemptID
	inputDir, err := os.MkdirTemp("", "agentos-input-*")
	if err != nil {
		return nil, fmt.Errorf("create workload input directory: %w", err)
	}
	inputPath := filepath.Join(inputDir, "workload.json")
	if err := os.WriteFile(inputPath, spec.WorkloadSpecJSON, 0o400); err != nil {
		_ = os.RemoveAll(inputDir)
		return nil, fmt.Errorf("persist workload spec: %w", err)
	}

	args := []string{"run", "--runtime", e.runtime}
	for _, mount := range e.mounts(inputPath, spec.WorkspaceBytes) {
		args = append(args, "--mount", mount)
	}
	for _, env := range e.environment(spec, "/agentos/input/workload.json") {
		args = append(args, "--env", env)
	}
	args = append(args, spec.ImageRef, containerID)

	// CommandContext ties the ctr process to the execution context: when the
	// lease keeper cancels the execution (cancellation or fence break), the
	// process is killed, Run returns, the spawn goroutine completes and the
	// staging directory is removed — no orphaned process or goroutine leak.
	command := exec.CommandContext(ctx, e.ctrPath, append([]string{"-n", e.namespace}, args...)...)
	var output limitedBuffer
	output.max = e.outputLimit
	command.Stdout, command.Stderr = &output, &output
	done := make(chan executionOutcome, 1)
	startedAt := time.Now()
	go func() {
		runErr := command.Run()
		result, _ := outcomeFor(runErr, startedAt)
		done <- executionOutcome{result: result, err: runErr}
		_ = os.RemoveAll(inputDir)
	}()
	return &ctrExecution{executor: e, containerID: containerID, done: done}, nil
}

func (e *ctrExecutor) Destroy(ctx context.Context, execution Execution) error {
	if err := e.run(ctx, "tasks", "delete", "-f", execution.ID()); err != nil && !notFound(err) {
		return fmt.Errorf("delete sandbox task: %w", err)
	}
	if err := e.run(ctx, "containers", "delete", execution.ID()); err != nil && !notFound(err) {
		return fmt.Errorf("delete sandbox container: %w", err)
	}
	return nil
}

// mounts builds the containerd mount flags: a read-only bind of the workload
// spec and a size-bounded tmpfs workspace. No host path other than the spec
// file is ever mounted into the sandbox.
func (e *ctrExecutor) mounts(inputPath string, workspaceBytes int64) []string {
	mounts := []string{
		"type=bind,src=" + inputPath + ",dst=/agentos/input/workload.json,options=rbind:ro",
	}
	if workspaceBytes > 0 {
		mounts = append(mounts, fmt.Sprintf("type=tmpfs,dst=/agentos/workspace,options=size=%d", workspaceBytes))
	}
	return mounts
}

func (e *ctrExecutor) environment(spec ExecutionSpec, inputPath string) []string {
	return []string{
		"AGENTOS_TENANT_ID=" + spec.TenantID,
		"AGENTOS_ATTEMPT_ID=" + spec.AttemptID,
		"AGENTOS_AGENT_VERSION_REF=" + spec.AgentVersionRef,
		"AGENTOS_INPUT_PATH=" + inputPath,
		"AGENTOS_WORKSPACE_PATH=/agentos/workspace",
	}
}

// run executes one ctr command with bounded captured output.
func (e *ctrExecutor) run(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, e.ctrPath, append([]string{"-n", e.namespace}, args...)...)
	var output limitedBuffer
	output.max = e.outputLimit
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("ctr %s: %w (output: %s)", strings.Join(args, " "), err, strings.TrimSpace(output.String()))
	}
	return nil
}

type executionOutcome struct {
	result RunResult
	err    error
}

// ctrExecution owns one foreground `ctr run` process; when the process exits,
// the task has terminated and ctr propagates the task exit code.
type ctrExecution struct {
	executor    *ctrExecutor
	containerID string
	done        chan executionOutcome
}

func (e *ctrExecution) ID() string { return e.containerID }

func (e *ctrExecution) Wait(ctx context.Context) (RunResult, error) {
	select {
	case outcome := <-e.done:
		return outcome.result, outcome.err
	case <-ctx.Done():
		killCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.executor.run(killCtx, "tasks", "kill", e.containerID)
		select {
		case outcome := <-e.done:
			return outcome.result, outcome.err
		case <-time.After(60 * time.Second):
			return RunResult{}, fmt.Errorf("sandbox %s did not terminate after kill; orphan cleanup required", e.containerID)
		}
	}
}

// outcomeFor maps a finished command to a RunResult. A task that exited
// non-zero surfaces as ExitCode with a nil error; a ctr machinery failure
// surfaces as an error so the worker reports execution_failed.
func outcomeFor(err error, startedAt time.Time) (RunResult, error) {
	result := RunResult{UsageMillis: time.Since(startedAt).Milliseconds()}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

// notFound reports whether a ctr failure was an object-not-found, which is
// benign during Destroy.
func notFound(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}

// limitedBuffer is a write-only buffer that truncates at max bytes so container
// or tool output can never grow unbounded in kernel memory.
type limitedBuffer struct {
	buffer bytes.Buffer
	max    int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.max - int64(b.buffer.Len())
	if remaining <= 0 {
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	return b.buffer.Write(p)
}

func (b *limitedBuffer) String() string { return b.buffer.String() }
