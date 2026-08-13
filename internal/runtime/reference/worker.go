// Package reference provides a deterministic Runtime Protocol conformance worker.
// It is a development and fault-injection provider, not a sandbox for untrusted code.
package reference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	runtimev1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/runtime/v1alpha1"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	ProviderName        = "reference-go"
	RuntimeABI          = "agentos.reference/v1"
	CheckpointSchema    = "agentos.reference-state/v1"
	checkpointMediaType = "application/vnd.agentos.reference-state+json"
	resultMediaType     = "application/vnd.agentos.reference-result+json"
)

type ArtifactStore interface {
	Put(context.Context, string, string, io.Reader) (store.ArtifactReference, error)
	Open(context.Context, string, store.ArtifactReference) (io.ReadCloser, error)
}

type Worker struct {
	client            runtimev1alpha1.RuntimeControlServiceClient
	artifacts         ArtifactStore
	tenantID          string
	runtimeInstanceID string
	heartbeatTTL      time.Duration
}

func NewWorker(client runtimev1alpha1.RuntimeControlServiceClient, artifacts ArtifactStore, tenantID, runtimeInstanceID string, heartbeatTTL time.Duration) *Worker {
	return &Worker{client: client, artifacts: artifacts, tenantID: tenantID, runtimeInstanceID: runtimeInstanceID, heartbeatTTL: heartbeatTTL}
}

func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	polled, err := w.client.PollAssignment(ctx, &runtimev1alpha1.PollAssignmentRequest{
		TenantId: w.tenantID, RuntimeInstanceId: w.runtimeInstanceID,
	})
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("poll runtime assignment: %w", err)
	}
	assignment := polled.GetAssignment()
	if assignment == nil || assignment.GetIdentity() == nil {
		return false, fmt.Errorf("runtime assignment is incomplete")
	}
	if assignment.GetRuntimeInstanceId() != w.runtimeInstanceID || assignment.GetIdentity().GetTenantId() != w.tenantID {
		return false, fmt.Errorf("runtime assignment identity does not match worker")
	}
	version := assignment.GetAttemptVersion()
	starting, err := w.transition(ctx, assignment.GetIdentity(), version,
		runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_STARTING, "starting")
	if err != nil {
		return false, err
	}
	version = starting.GetAttemptVersion()
	running, err := w.transition(ctx, assignment.GetIdentity(), version,
		runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_RUNNING, "running")
	if err != nil {
		return false, err
	}
	version = running.GetAttemptVersion()

	heartbeat, err := w.client.Heartbeat(ctx, &runtimev1alpha1.HeartbeatRequest{
		Identity: assignment.GetIdentity(), ExpectedLeaseVersion: assignment.GetLeaseVersion(),
		IdempotencyKey: operationKey(assignment, "heartbeat"), RequestedTtlSeconds: int64(w.heartbeatTTL / time.Second),
	})
	if err != nil {
		return false, fmt.Errorf("renew runtime lease: %w", err)
	}
	if heartbeat.GetCancelRequested() {
		_, err := w.client.AcknowledgeCancellation(ctx, &runtimev1alpha1.AcknowledgeCancellationRequest{
			Identity: assignment.GetIdentity(), ExpectedAttemptVersion: heartbeat.GetAttemptVersion(),
			IdempotencyKey: operationKey(assignment, "cancel"),
		})
		if err != nil {
			return false, fmt.Errorf("acknowledge runtime cancellation: %w", err)
		}
		return true, nil
	}

	goalDigest := sha256.Sum256([]byte(assignment.GetGoal()))
	if assignment.GetResumeCheckpoint() == nil {
		state, err := json.Marshal(checkpointState{GoalSHA256: hex.EncodeToString(goalDigest[:]), Step: "prepared"})
		if err != nil {
			return false, err
		}
		stateArtifact, err := w.artifacts.Put(ctx, w.tenantID, checkpointMediaType, bytes.NewReader(state))
		if err != nil {
			return false, fmt.Errorf("persist logical checkpoint: %w", err)
		}
		checkpointID, err := uuid.NewV7()
		if err != nil {
			return false, fmt.Errorf("create checkpoint ID: %w", err)
		}
		committed, err := w.client.CommitCheckpoint(ctx, &runtimev1alpha1.CommitCheckpointRequest{
			Identity: assignment.GetIdentity(), ExpectedAttemptVersion: version,
			IdempotencyKey: operationKey(assignment, "checkpoint-prepared"), CheckpointId: checkpointID.String(),
			AgentVersionRef: assignment.GetAgentVersionRef(), Provider: ProviderName, RuntimeAbi: RuntimeABI,
			SchemaVersion: CheckpointSchema, State: artifactProto(stateArtifact),
		})
		if err != nil {
			return false, fmt.Errorf("commit logical checkpoint: %w", err)
		}
		version = committed.GetAttemptVersion()
	} else if err := w.restoreCheckpoint(ctx, assignment, goalDigest); err != nil {
		return false, err
	}

	resultDocument, err := json.Marshal(map[string]any{
		"agentVersionRef": assignment.GetAgentVersionRef(), "attemptId": assignment.GetIdentity().GetAttemptId(),
		"goal": assignment.GetGoal(), "provider": ProviderName, "resumed": assignment.GetResumeCheckpoint() != nil,
	})
	if err != nil {
		return false, err
	}
	resultArtifact, err := w.artifacts.Put(ctx, w.tenantID, resultMediaType, bytes.NewReader(resultDocument))
	if err != nil {
		return false, fmt.Errorf("persist runtime result: %w", err)
	}
	_, err = w.client.CompleteAttempt(ctx, &runtimev1alpha1.CompleteAttemptRequest{
		Identity: assignment.GetIdentity(), ExpectedAttemptVersion: version,
		IdempotencyKey: operationKey(assignment, "complete"), Result: artifactProto(resultArtifact),
	})
	if err != nil {
		return false, fmt.Errorf("complete runtime attempt: %w", err)
	}
	return true, nil
}

func (w *Worker) transition(ctx context.Context, identity *runtimev1alpha1.AttemptIdentity, version int64, phase runtimev1alpha1.AttemptPhase, operation string) (*runtimev1alpha1.TransitionAttemptResponse, error) {
	response, err := w.client.TransitionAttempt(ctx, &runtimev1alpha1.TransitionAttemptRequest{
		Identity: identity, ExpectedAttemptVersion: version,
		IdempotencyKey: identity.GetAttemptId() + ":" + operation, TargetPhase: phase,
	})
	if err != nil {
		return nil, fmt.Errorf("transition attempt to %s: %w", phase, err)
	}
	return response, nil
}

func (w *Worker) restoreCheckpoint(ctx context.Context, assignment *runtimev1alpha1.Assignment, goalDigest [sha256.Size]byte) error {
	checkpoint := assignment.GetResumeCheckpoint()
	if checkpoint.GetAgentVersionRef() != assignment.GetAgentVersionRef() || checkpoint.GetRuntimeClass() != assignment.GetRuntimeClass() ||
		checkpoint.GetProvider() != ProviderName || checkpoint.GetRuntimeAbi() != RuntimeABI || checkpoint.GetSchemaVersion() != CheckpointSchema {
		return fmt.Errorf("checkpoint is not compatible with reference runtime assignment")
	}
	reference, err := artifactFromProto(checkpoint.GetState())
	if err != nil {
		return err
	}
	reader, err := w.artifacts.Open(ctx, w.tenantID, reference)
	if err != nil {
		return fmt.Errorf("open checkpoint state: %w", err)
	}
	defer reader.Close()
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	decoder.DisallowUnknownFields()
	var state checkpointState
	if err := decoder.Decode(&state); err != nil {
		return fmt.Errorf("decode checkpoint state: %w", err)
	}
	if state.Step != "prepared" || !strings.EqualFold(state.GoalSHA256, hex.EncodeToString(goalDigest[:])) {
		return fmt.Errorf("checkpoint logical state does not match assigned task")
	}
	return nil
}

type checkpointState struct {
	GoalSHA256 string `json:"goalSha256"`
	Step       string `json:"step"`
}

func operationKey(assignment *runtimev1alpha1.Assignment, operation string) string {
	return assignment.GetIdentity().GetAttemptId() + ":" + operation
}

func artifactProto(reference store.ArtifactReference) *runtimev1alpha1.ArtifactReference {
	return &runtimev1alpha1.ArtifactReference{
		Uri: reference.URI, Sha256: reference.DigestHex(), SizeBytes: reference.SizeBytes, MediaType: reference.MediaType,
	}
}

func artifactFromProto(reference *runtimev1alpha1.ArtifactReference) (store.ArtifactReference, error) {
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
