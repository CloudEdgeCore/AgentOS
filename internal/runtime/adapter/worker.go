// Package adapter implements the Runtime Protocol provider that delegates an
// assignment to a language/framework-neutral Agent Runtime Interface endpoint.
package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	runtimev1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/runtime/v1"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/agentversion"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/CloudEdgeCore/AgentOS/internal/mcp"
	"github.com/CloudEdgeCore/AgentOS/internal/runtime/leasekeeper"
	"github.com/CloudEdgeCore/AgentOS/sdk/agent"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	ProviderName        = "adapter-http"
	CheckpointSchema    = "agentos.adapter-checkpoint/v1"
	checkpointMediaType = "application/vnd.agentos.adapter-checkpoint+json"
	resultMediaType     = "application/vnd.agentos.adapter-result+json"
	controlRPCTimeout   = 15 * time.Second
)

type ArtifactStore interface {
	Put(context.Context, string, string, io.Reader) (store.ArtifactReference, error)
	Open(context.Context, string, store.ArtifactReference) (io.ReadCloser, error)
}

// ExecutionWindow receives the fenced execution context for the duration of
// one assignment: the runtime opens the sandbox Agent's brokered access
// (MCP) when the attempt is RUNNING and closes it on every exit path
// (default deny outside execution windows). Open returns the closer the
// worker defers, so concurrent workers sharing one Agent endpoint bind
// their identities per attempt.
type ExecutionWindow interface {
	Open(mcp.AttemptContext) func()
}

type Worker struct {
	control           runtimev1.RuntimeControlServiceClient
	artifacts         ArtifactStore
	runtime           *agent.Client
	endpoint          string
	tenantID          string
	runtimeInstanceID string
	heartbeatTTL      time.Duration
	pollInterval      time.Duration
	window            ExecutionWindow
}

// WithExecutionWindow attaches the sandbox brokered-access window (the
// loopback MCP identity slot). The worker publishes the fenced identity and
// the AgentVersion capability grants for the duration of each assignment.
func (w *Worker) WithExecutionWindow(window ExecutionWindow) *Worker {
	w.window = window
	return w
}

func NewWorker(
	control runtimev1.RuntimeControlServiceClient,
	artifacts ArtifactStore,
	endpoint, tenantID, runtimeInstanceID string,
	heartbeatTTL time.Duration,
	httpClient *http.Client,
) (*Worker, error) {
	if control == nil || artifacts == nil {
		return nil, fmt.Errorf("Runtime Protocol client and artifact store are required")
	}
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(runtimeInstanceID) == "" || heartbeatTTL <= 0 {
		return nil, fmt.Errorf("tenant, runtime instance and positive heartbeat TTL are required")
	}
	client, err := agent.NewClient(endpoint, httpClient)
	if err != nil {
		return nil, err
	}
	return &Worker{
		control: control, artifacts: artifacts, runtime: client, endpoint: strings.TrimRight(endpoint, "/"),
		tenantID: tenantID, runtimeInstanceID: runtimeInstanceID, heartbeatTTL: heartbeatTTL,
		pollInterval: 25 * time.Millisecond,
	}, nil
}

func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	pollCtx, cancel := context.WithTimeout(ctx, controlRPCTimeout)
	polled, err := w.control.PollAssignment(pollCtx, &runtimev1.PollAssignmentRequest{
		TenantId: w.tenantID, RuntimeInstanceId: w.runtimeInstanceID,
	})
	cancel()
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("poll adapter assignment: %w", err)
	}
	assignment := polled.GetAssignment()
	if assignment == nil || assignment.GetIdentity() == nil {
		return false, fmt.Errorf("runtime assignment is incomplete")
	}
	if assignment.GetIdentity().GetTenantId() != w.tenantID || assignment.GetRuntimeInstanceId() != w.runtimeInstanceID {
		return false, fmt.Errorf("runtime assignment identity does not match adapter worker")
	}
	target, capabilities, checkpointPolicy, err := w.target(assignment)
	if err != nil {
		return w.fail(ctx, assignment.GetIdentity(), assignment.GetAttemptVersion(), "adapter_manifest_invalid", err)
	}
	identity := assignment.GetIdentity()
	version, err := w.transition(ctx, identity, assignment.GetAttemptVersion(),
		runtimev1.AttemptPhase_ATTEMPT_PHASE_STARTING, "", "")
	if err != nil {
		if status.Code(err) == codes.Aborted {
			return false, nil
		}
		return false, err
	}
	version, err = w.transition(ctx, identity, version,
		runtimev1.AttemptPhase_ATTEMPT_PHASE_RUNNING, "", "")
	if err != nil {
		return false, err
	}
	if w.window != nil {
		closeWindow := w.window.Open(mcp.AttemptContext{
			TenantID: identity.GetTenantId(), TaskID: parseAssignmentUUID(assignment.GetTaskId()),
			RunID: parseAssignmentUUID(assignment.GetRunId()), AttemptID: parseAssignmentUUID(identity.GetAttemptId()),
			FencingToken: identity.GetFencingToken(), AgentVersionRef: assignment.GetAgentVersionRef(),
			AllowedModels:           cloneExplicit(capabilities.Models),
			AllowedMemoryNamespaces: cloneExplicit(capabilities.Memory),
		})
		defer closeWindow()
	}
	heartbeat, err := w.heartbeat(ctx, assignment)
	if err != nil {
		return false, err
	}
	if heartbeat.GetCancelRequested() {
		return true, nil
	}
	keeper, executionCtx := leasekeeper.Start(ctx, leasekeeper.Options{
		Client: w.control, Identity: identity, AttemptID: identity.GetAttemptId(),
		HeartbeatTTL: w.heartbeatTTL, RPCTimeout: controlRPCTimeout,
	}, heartbeat.GetLeaseVersion(), heartbeat.GetAttemptVersion())
	defer keeper.Stop()

	if assignment.GetResumeCheckpoint() != nil {
		if checkpointPolicy.Mode != agentversion.CheckpointLogical {
			return w.fail(ctx, identity, version, "adapter_restore_failed", fmt.Errorf("checkpoint resume is disabled by the AgentVersion manifest"))
		}
		if err := w.restore(executionCtx, assignment, target, checkpointPolicy); err != nil {
			return w.fail(ctx, identity, version, "adapter_restore_failed", err)
		}
	}
	start := agent.StartRequest{
		ExecutionID: identity.GetAttemptId(), AgentVersionRef: assignment.GetAgentVersionRef(),
		Goal: assignment.GetGoal(), Input: json.RawMessage(assignment.GetWorkloadSpecJson()),
		Capabilities: capabilities,
	}
	if _, err := w.runtime.Start(executionCtx, start); err != nil {
		if keeper.Cancelled() {
			return true, nil
		}
		return w.fail(ctx, identity, version, "adapter_start_failed", err)
	}
	checkpointSequence := 0
	result, events, err := w.wait(executionCtx, identity.GetAttemptId(),
		time.Duration(checkpointPolicy.IntervalSeconds)*time.Second, func() error {
			checkpointSequence++
			nextVersion, checkpointErr := w.commitLogicalCheckpoint(executionCtx, assignment, target,
				checkpointPolicy, version, fmt.Sprintf("checkpoint-%d", checkpointSequence))
			if checkpointErr == nil {
				version = nextVersion
			}
			return checkpointErr
		})
	if err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = w.runtime.Stop(stopCtx, identity.GetAttemptId())
		stopCancel()
		if keeper.Cancelled() {
			return true, nil
		}
		if fenceErr := keeper.FenceError(); fenceErr != nil {
			return false, fenceErr
		}
		return w.fail(ctx, identity, version, "adapter_execution_failed", err)
	}
	if result.Status != agent.StatusSucceeded {
		return w.fail(ctx, identity, version, "adapter_result_"+strings.ToLower(result.Status),
			fmt.Errorf("%s: %s", result.ErrorCode, result.Error))
	}
	if keeper.Cancelled() {
		return true, nil
	}
	if checkpointPolicy.Mode == agentversion.CheckpointLogical {
		version, err = w.commitLogicalCheckpoint(executionCtx, assignment, target, checkpointPolicy, version, "checkpoint-final")
		if err != nil {
			return w.fail(ctx, identity, version, "adapter_checkpoint_failed", err)
		}
	}
	resultDocument, err := json.Marshal(map[string]any{
		"agentVersionRef": assignment.GetAgentVersionRef(), "attemptId": identity.GetAttemptId(),
		"provider": ProviderName, "adapterEndpoint": w.endpoint, "runtimeABI": target.RuntimeABI,
		"output": result.Output, "events": events, "resumed": assignment.GetResumeCheckpoint() != nil,
		"checkpointMode": checkpointPolicy.Mode,
	})
	if err != nil {
		return false, err
	}
	resultArtifact, err := w.artifacts.Put(executionCtx, w.tenantID, resultMediaType, bytes.NewReader(resultDocument))
	if err != nil {
		return false, fmt.Errorf("persist adapter result: %w", err)
	}
	if _, err := w.control.CompleteAttempt(executionCtx, &runtimev1.CompleteAttemptRequest{
		Identity: identity, ExpectedAttemptVersion: version,
		IdempotencyKey: operationKey(assignment, "complete"), Result: artifactProto(resultArtifact),
	}); err != nil {
		return false, fmt.Errorf("complete adapter attempt: %w", err)
	}
	return true, nil
}

func (w *Worker) target(assignment *runtimev1.Assignment) (agentversion.RuntimeTarget, agent.CapabilityGrant, agentversion.CheckpointPolicy, error) {
	var spec agentversion.Spec
	decoder := json.NewDecoder(bytes.NewReader(assignment.GetAgentVersionSpecJson()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return agentversion.RuntimeTarget{}, agent.CapabilityGrant{}, agentversion.CheckpointPolicy{}, fmt.Errorf("decode immutable AgentVersion spec: %w", err)
	}
	if err := agentversion.ValidateSpec(assignment.GetAgentVersionSpecJson()); err != nil {
		return agentversion.RuntimeTarget{}, agent.CapabilityGrant{}, agentversion.CheckpointPolicy{}, err
	}
	for _, target := range spec.Runtimes {
		if target.Class != assignment.GetRuntimeClass() {
			continue
		}
		if (target.Interface != agentversion.RuntimeInterfaceV1 && target.Interface != agentversion.RuntimeInterfaceV1Alpha1) || len(target.Entrypoint) != 1 ||
			strings.TrimRight(target.Entrypoint[0], "/") != w.endpoint {
			return agentversion.RuntimeTarget{}, agent.CapabilityGrant{}, agentversion.CheckpointPolicy{},
				fmt.Errorf("runtime target does not bind the configured Runtime Interface endpoint")
		}
		if spec.Capabilities == nil {
			return agentversion.RuntimeTarget{}, agent.CapabilityGrant{}, agentversion.CheckpointPolicy{}, fmt.Errorf("capability declaration is required")
		}
		if spec.Checkpoint == nil {
			return agentversion.RuntimeTarget{}, agent.CapabilityGrant{}, agentversion.CheckpointPolicy{}, fmt.Errorf("checkpoint policy is required")
		}
		return target, agent.CapabilityGrant{
			Tools: cloneExplicit(spec.Capabilities.Tools), Models: cloneExplicit(spec.Capabilities.Models),
			Memory: cloneExplicit(spec.Capabilities.Memory), Secrets: cloneExplicit(spec.Capabilities.Secrets),
		}, *spec.Checkpoint, nil
	}
	return agentversion.RuntimeTarget{}, agent.CapabilityGrant{}, agentversion.CheckpointPolicy{}, fmt.Errorf("no runtime target for class %q", assignment.GetRuntimeClass())
}

func cloneExplicit(values []string) []string {
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

// parseAssignmentUUID converts proto string ids; a malformed id resolves to
// the zero UUID and the brokered tools deny closed on it.
func parseAssignmentUUID(value string) uuid.UUID {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil
	}
	return parsed
}

func (w *Worker) wait(ctx context.Context, executionID string, checkpointInterval time.Duration, checkpoint func() error) (agent.Result, []agent.Event, error) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	var checkpointTicker *time.Ticker
	var checkpointTick <-chan time.Time
	if checkpointInterval > 0 {
		checkpointTicker = time.NewTicker(checkpointInterval)
		checkpointTick = checkpointTicker.C
		defer checkpointTicker.Stop()
	}
	var events []agent.Event
	var after int64
	for {
		list, err := w.runtime.Events(ctx, executionID, after)
		if err != nil {
			return agent.Result{}, nil, err
		}
		events = append(events, list.Events...)
		after = list.NextAfter
		result, terminal, err := w.runtime.Result(ctx, executionID)
		if err != nil {
			return agent.Result{}, nil, err
		}
		if terminal {
			return result, events, nil
		}
		select {
		case <-ctx.Done():
			return agent.Result{}, nil, ctx.Err()
		case <-ticker.C:
		case <-checkpointTick:
			if checkpoint == nil {
				return agent.Result{}, nil, fmt.Errorf("periodic checkpoint callback is not configured")
			}
			if err := checkpoint(); err != nil {
				return agent.Result{}, nil, fmt.Errorf("periodic checkpoint: %w", err)
			}
		}
	}
}

func (w *Worker) commitLogicalCheckpoint(
	ctx context.Context,
	assignment *runtimev1.Assignment,
	target agentversion.RuntimeTarget,
	policy agentversion.CheckpointPolicy,
	version int64,
	operation string,
) (int64, error) {
	checkpoint, err := w.runtime.Checkpoint(ctx, assignment.GetIdentity().GetAttemptId())
	if err != nil {
		return version, err
	}
	if checkpoint.Checkpoint.SchemaVersion != policy.SchemaVersion {
		return version, fmt.Errorf("runtime checkpoint schema %q does not match manifest schema %q",
			checkpoint.Checkpoint.SchemaVersion, policy.SchemaVersion)
	}
	document, err := json.Marshal(checkpoint.Checkpoint)
	if err != nil {
		return version, err
	}
	artifact, err := w.artifacts.Put(ctx, w.tenantID, checkpointMediaType, bytes.NewReader(document))
	if err != nil {
		return version, fmt.Errorf("persist adapter checkpoint: %w", err)
	}
	checkpointID, err := uuid.NewV7()
	if err != nil {
		return version, err
	}
	committed, err := w.control.CommitCheckpoint(ctx, &runtimev1.CommitCheckpointRequest{
		Identity: assignment.GetIdentity(), ExpectedAttemptVersion: version,
		IdempotencyKey: operationKey(assignment, operation), CheckpointId: checkpointID.String(),
		AgentVersionRef: assignment.GetAgentVersionRef(), Provider: ProviderName,
		RuntimeAbi: target.RuntimeABI, SchemaVersion: CheckpointSchema, State: artifactProto(artifact),
	})
	if err != nil {
		return version, fmt.Errorf("commit adapter checkpoint: %w", err)
	}
	return committed.GetAttemptVersion(), nil
}

func (w *Worker) restore(ctx context.Context, assignment *runtimev1.Assignment, target agentversion.RuntimeTarget, policy agentversion.CheckpointPolicy) error {
	checkpoint := assignment.GetResumeCheckpoint()
	if checkpoint.GetAgentVersionRef() != assignment.GetAgentVersionRef() ||
		checkpoint.GetRuntimeClass() != assignment.GetRuntimeClass() ||
		checkpoint.GetProvider() != ProviderName ||
		checkpoint.GetRuntimeAbi() != target.RuntimeABI ||
		checkpoint.GetSchemaVersion() != CheckpointSchema {
		return fmt.Errorf("checkpoint is incompatible with adapter assignment")
	}
	reference, err := artifactFromProto(checkpoint.GetState())
	if err != nil {
		return err
	}
	reader, err := w.artifacts.Open(ctx, w.tenantID, reference)
	if err != nil {
		return err
	}
	defer reader.Close()
	decoder := json.NewDecoder(io.LimitReader(reader, 2<<20))
	decoder.DisallowUnknownFields()
	var sdkCheckpoint agent.Checkpoint
	if err := decoder.Decode(&sdkCheckpoint); err != nil {
		return fmt.Errorf("decode adapter checkpoint: %w", err)
	}
	if sdkCheckpoint.SchemaVersion != policy.SchemaVersion {
		return fmt.Errorf("checkpoint logical schema does not match the AgentVersion manifest")
	}
	_, err = w.runtime.Restore(ctx, agent.RestoreRequest{
		ExecutionID: assignment.GetIdentity().GetAttemptId(), Checkpoint: sdkCheckpoint,
	})
	return err
}

func (w *Worker) heartbeat(ctx context.Context, assignment *runtimev1.Assignment) (*runtimev1.HeartbeatResponse, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, controlRPCTimeout)
	defer cancel()
	response, err := w.control.Heartbeat(rpcCtx, &runtimev1.HeartbeatRequest{
		Identity: assignment.GetIdentity(), ExpectedLeaseVersion: assignment.GetLeaseVersion(),
		IdempotencyKey: operationKey(assignment, "heartbeat"), RequestedTtlSeconds: int64(w.heartbeatTTL / time.Second),
	})
	if err != nil {
		return nil, fmt.Errorf("renew adapter lease: %w", err)
	}
	if response.GetCancelRequested() {
		_, err := w.control.AcknowledgeCancellation(rpcCtx, &runtimev1.AcknowledgeCancellationRequest{
			Identity: assignment.GetIdentity(), ExpectedAttemptVersion: response.GetAttemptVersion(),
			IdempotencyKey: operationKey(assignment, "cancel"),
		})
		if err != nil {
			return nil, fmt.Errorf("acknowledge adapter cancellation: %w", err)
		}
	}
	return response, nil
}

func (w *Worker) transition(
	ctx context.Context,
	identity *runtimev1.AttemptIdentity,
	version int64,
	phase runtimev1.AttemptPhase,
	failureCode, failureMessage string,
) (int64, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, controlRPCTimeout)
	defer cancel()
	response, err := w.control.TransitionAttempt(rpcCtx, &runtimev1.TransitionAttemptRequest{
		Identity: identity, ExpectedAttemptVersion: version,
		IdempotencyKey: identity.GetAttemptId() + ":" + strings.ToLower(phase.String()),
		TargetPhase:    phase, FailureCode: failureCode, FailureMessage: failureMessage,
	})
	if err != nil {
		return 0, fmt.Errorf("transition adapter attempt to %s: %w", phase, err)
	}
	return response.GetAttemptVersion(), nil
}

func (w *Worker) fail(
	ctx context.Context,
	identity *runtimev1.AttemptIdentity,
	version int64,
	code string,
	cause error,
) (bool, error) {
	if _, err := w.transition(ctx, identity, version,
		runtimev1.AttemptPhase_ATTEMPT_PHASE_FAILED, code, cause.Error()); err != nil {
		return false, fmt.Errorf("mark adapter attempt failed (%s): %w", code, err)
	}
	return true, nil
}

func operationKey(assignment *runtimev1.Assignment, operation string) string {
	return assignment.GetIdentity().GetAttemptId() + ":adapter:" + operation
}

func artifactProto(reference store.ArtifactReference) *runtimev1.ArtifactReference {
	return &runtimev1.ArtifactReference{
		Uri: reference.URI, Sha256: reference.DigestHex(), SizeBytes: reference.SizeBytes, MediaType: reference.MediaType,
	}
}

func artifactFromProto(reference *runtimev1.ArtifactReference) (store.ArtifactReference, error) {
	var result store.ArtifactReference
	if reference == nil {
		return result, fmt.Errorf("checkpoint artifact reference is required")
	}
	digest, err := hex.DecodeString(reference.GetSha256())
	if err != nil || len(digest) != sha256.Size {
		return result, fmt.Errorf("checkpoint artifact digest is invalid")
	}
	copy(result.SHA256[:], digest)
	result.URI, result.SizeBytes, result.MediaType = reference.GetUri(), reference.GetSizeBytes(), reference.GetMediaType()
	return result, result.Validate()
}
