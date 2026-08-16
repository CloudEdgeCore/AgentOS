// Package control implements the fenced Kernel-to-Runtime gRPC boundary.
package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	runtimev1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/runtime/v1alpha1"
	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Service struct {
	runtimev1alpha1.UnimplementedRuntimeControlServiceServer
	store         store.RuntimeStore
	allowedTenant string
	maxLeaseTTL   time.Duration
}

func NewService(repository store.RuntimeStore, allowedTenant string, maxLeaseTTL time.Duration) *Service {
	return &Service{store: repository, allowedTenant: allowedTenant, maxLeaseTTL: maxLeaseTTL}
}

func (s *Service) PollAssignment(ctx context.Context, request *runtimev1alpha1.PollAssignmentRequest) (*runtimev1alpha1.PollAssignmentResponse, error) {
	if request == nil || strings.TrimSpace(request.GetRuntimeInstanceId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "runtime instance ID is required")
	}
	if err := s.authorizeTenant(request.GetTenantId()); err != nil {
		return nil, err
	}
	assignment, err := s.store.PollRuntimeAssignment(ctx, request.GetTenantId(), request.GetRuntimeInstanceId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &runtimev1alpha1.PollAssignmentResponse{Assignment: assignmentProto(assignment)}, nil
}

func (s *Service) GetAssignment(ctx context.Context, request *runtimev1alpha1.GetAssignmentRequest) (*runtimev1alpha1.GetAssignmentResponse, error) {
	identity, attemptID, err := s.parseIdentity(request.GetIdentity())
	if err != nil {
		return nil, err
	}
	assignment, err := s.store.GetRuntimeAssignment(ctx, identity.GetTenantId(), attemptID, identity.GetFencingToken())
	if err != nil {
		return nil, rpcError(err)
	}
	return &runtimev1alpha1.GetAssignmentResponse{Assignment: assignmentProto(assignment)}, nil
}

func (s *Service) TransitionAttempt(ctx context.Context, request *runtimev1alpha1.TransitionAttemptRequest) (*runtimev1alpha1.TransitionAttemptResponse, error) {
	identity, attemptID, err := s.parseIdentity(request.GetIdentity())
	if err != nil {
		return nil, err
	}
	if request.GetExpectedAttemptVersion() <= 0 || strings.TrimSpace(request.GetIdempotencyKey()) == "" {
		return nil, status.Error(codes.InvalidArgument, "expected attempt version and idempotency key are required")
	}
	target, err := domainPhase(request.GetTargetPhase())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	updated, err := s.store.TransitionAttempt(ctx, store.TransitionAttemptInput{
		AttemptID: attemptID, FencingToken: identity.GetFencingToken(),
		ExpectedAttemptVersion: request.GetExpectedAttemptVersion(), To: target,
		FailureCode: request.GetFailureCode(), FailureMessage: request.GetFailureMessage(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &runtimev1alpha1.TransitionAttemptResponse{
		Phase: protoPhase(updated.Phase), AttemptVersion: updated.ResourceVersion,
	}, nil
}

func (s *Service) Heartbeat(ctx context.Context, request *runtimev1alpha1.HeartbeatRequest) (*runtimev1alpha1.HeartbeatResponse, error) {
	identity, attemptID, err := s.parseIdentity(request.GetIdentity())
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(request.GetRequestedTtlSeconds()) * time.Second
	if request.GetExpectedLeaseVersion() <= 0 || strings.TrimSpace(request.GetIdempotencyKey()) == "" || ttl <= 0 || ttl > s.maxLeaseTTL {
		return nil, status.Error(codes.InvalidArgument, "valid lease version, idempotency key, and bounded TTL are required")
	}
	lease, err := s.store.HeartbeatLease(ctx, store.HeartbeatLeaseInput{
		AttemptID: attemptID, FencingToken: identity.GetFencingToken(),
		ExpectedLeaseVersion: request.GetExpectedLeaseVersion(), TTL: ttl,
	})
	if err != nil {
		return nil, rpcError(err)
	}
	// The narrow renewal read answers the cancel check without
	// re-materializing the full assignment on every heartbeat.
	status, err := s.store.GetHeartbeatStatus(ctx, identity.GetTenantId(), attemptID, identity.GetFencingToken())
	if err != nil {
		return nil, rpcError(err)
	}
	return &runtimev1alpha1.HeartbeatResponse{
		LeaseVersion: lease.ResourceVersion, ExpiresAt: timestamppb.New(lease.ExpiresAt),
		CancelRequested: status.CancelRequested, AttemptVersion: status.AttemptVersion,
	}, nil
}

func (s *Service) CommitCheckpoint(ctx context.Context, request *runtimev1alpha1.CommitCheckpointRequest) (*runtimev1alpha1.CommitCheckpointResponse, error) {
	identity, attemptID, err := s.parseIdentity(request.GetIdentity())
	if err != nil {
		return nil, err
	}
	checkpointID, err := uuid.Parse(request.GetCheckpointId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "checkpoint ID must be a UUID")
	}
	artifact, err := artifactFromProto(request.GetState())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	checkpoint, attempt, err := s.store.CommitCheckpoint(ctx, store.CommitCheckpointInput{
		TenantID: identity.GetTenantId(), AttemptID: attemptID, FencingToken: identity.GetFencingToken(),
		ExpectedAttemptVersion: request.GetExpectedAttemptVersion(), IdempotencyKey: request.GetIdempotencyKey(),
		CheckpointID: checkpointID, AgentVersionRef: request.GetAgentVersionRef(), Provider: request.GetProvider(),
		RuntimeABI: request.GetRuntimeAbi(), SchemaVersion: request.GetSchemaVersion(), State: artifact,
		ConfirmedReceiptIDs: request.GetConfirmedReceiptIds(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &runtimev1alpha1.CommitCheckpointResponse{
		Checkpoint: checkpointProto(checkpoint), AttemptVersion: attempt.ResourceVersion,
	}, nil
}

func (s *Service) CompleteAttempt(ctx context.Context, request *runtimev1alpha1.CompleteAttemptRequest) (*runtimev1alpha1.CompleteAttemptResponse, error) {
	identity, attemptID, err := s.parseIdentity(request.GetIdentity())
	if err != nil {
		return nil, err
	}
	artifact, err := artifactFromProto(request.GetResult())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := s.store.CompleteAttempt(ctx, store.CompleteAttemptInput{
		TenantID: identity.GetTenantId(), AttemptID: attemptID, FencingToken: identity.GetFencingToken(),
		ExpectedAttemptVersion: request.GetExpectedAttemptVersion(), IdempotencyKey: request.GetIdempotencyKey(), Result: artifact,
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &runtimev1alpha1.CompleteAttemptResponse{
		AttemptVersion: result.Attempt.ResourceVersion, RunVersion: result.Run.ResourceVersion,
		TaskVersion: result.Task.ResourceVersion, ResultRef: result.Task.ResultRef,
	}, nil
}

func (s *Service) AcknowledgeCancellation(ctx context.Context, request *runtimev1alpha1.AcknowledgeCancellationRequest) (*runtimev1alpha1.AcknowledgeCancellationResponse, error) {
	identity, attemptID, err := s.parseIdentity(request.GetIdentity())
	if err != nil {
		return nil, err
	}
	result, err := s.store.AcknowledgeCancellation(ctx, store.CancelAttemptInput{
		TenantID: identity.GetTenantId(), AttemptID: attemptID, FencingToken: identity.GetFencingToken(),
		ExpectedAttemptVersion: request.GetExpectedAttemptVersion(), IdempotencyKey: request.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &runtimev1alpha1.AcknowledgeCancellationResponse{
		AttemptVersion: result.Attempt.ResourceVersion, RunVersion: result.Run.ResourceVersion,
		TaskVersion: result.Task.ResourceVersion,
	}, nil
}

func (s *Service) parseIdentity(identity *runtimev1alpha1.AttemptIdentity) (*runtimev1alpha1.AttemptIdentity, uuid.UUID, error) {
	if identity == nil || identity.GetFencingToken() <= 0 {
		return nil, uuid.Nil, status.Error(codes.InvalidArgument, "attempt identity and positive fencing token are required")
	}
	if err := s.authorizeTenant(identity.GetTenantId()); err != nil {
		return nil, uuid.Nil, err
	}
	attemptID, err := uuid.Parse(identity.GetAttemptId())
	if err != nil {
		return nil, uuid.Nil, status.Error(codes.InvalidArgument, "attempt ID must be a UUID")
	}
	return identity, attemptID, nil
}

func (s *Service) authorizeTenant(tenantID string) error {
	if strings.TrimSpace(tenantID) == "" || tenantID != s.allowedTenant {
		return status.Error(codes.PermissionDenied, "tenant is not authorized by this runtime endpoint")
	}
	return nil
}

func assignmentProto(assignment store.RuntimeAssignment) *runtimev1alpha1.Assignment {
	result := &runtimev1alpha1.Assignment{
		Identity: &runtimev1alpha1.AttemptIdentity{
			TenantId: assignment.Attempt.TenantID, AttemptId: assignment.Attempt.ID.String(),
			FencingToken: assignment.Attempt.FencingToken,
		},
		RunId: assignment.Run.ID.String(), TaskId: assignment.Task.ID.String(),
		AgentVersionRef: assignment.Task.AgentVersionRef, Goal: assignment.Task.Goal,
		WorkloadSpecJson: append([]byte(nil), assignment.Task.Spec...), RuntimeClass: assignment.Attempt.RuntimeClass,
		RuntimePoolId: assignment.Attempt.RuntimePoolID, RuntimeInstanceId: assignment.Attempt.RuntimeInstanceID,
		AttemptVersion: assignment.Attempt.ResourceVersion, LeaseVersion: assignment.Lease.ResourceVersion,
		LeaseExpiresAt: timestamppb.New(assignment.Lease.ExpiresAt), Phase: string(assignment.Attempt.Phase),
	}
	if assignment.PendingApprovalID != nil {
		result.ApprovalId = assignment.PendingApprovalID.String()
	}
	if assignment.ResumeCheckpoint != nil {
		result.ResumeCheckpoint = checkpointProto(*assignment.ResumeCheckpoint)
	}
	return result
}

func checkpointProto(checkpoint store.Checkpoint) *runtimev1alpha1.CheckpointReference {
	return &runtimev1alpha1.CheckpointReference{
		CheckpointId: checkpoint.ID.String(), AgentVersionRef: checkpoint.AgentVersionRef,
		RuntimeClass: checkpoint.RuntimeClass, Provider: checkpoint.Provider, RuntimeAbi: checkpoint.RuntimeABI,
		SchemaVersion: checkpoint.SchemaVersion, State: artifactProto(checkpoint.State),
		ConfirmedReceiptIds: append([]string(nil), checkpoint.ConfirmedReceiptIDs...),
		EnvelopeSha256:      hex.EncodeToString(checkpoint.EnvelopeSHA256[:]), CreatedAt: timestamppb.New(checkpoint.CreatedAt),
	}
}

func artifactProto(artifact store.ArtifactReference) *runtimev1alpha1.ArtifactReference {
	return &runtimev1alpha1.ArtifactReference{
		Uri: artifact.URI, Sha256: artifact.DigestHex(), SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType,
	}
}

func artifactFromProto(artifact *runtimev1alpha1.ArtifactReference) (store.ArtifactReference, error) {
	var result store.ArtifactReference
	if artifact == nil {
		return result, fmt.Errorf("artifact reference is required")
	}
	digest, err := hex.DecodeString(artifact.GetSha256())
	if err != nil || len(digest) != sha256.Size {
		return result, fmt.Errorf("artifact SHA-256 must be 64 hexadecimal characters")
	}
	copy(result.SHA256[:], digest)
	result.URI = artifact.GetUri()
	result.SizeBytes = artifact.GetSizeBytes()
	result.MediaType = artifact.GetMediaType()
	return result, result.Validate()
}

func domainPhase(phase runtimev1alpha1.AttemptPhase) (domain.AttemptPhase, error) {
	switch phase {
	case runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_STARTING:
		return domain.AttemptStarting, nil
	case runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_RUNNING:
		return domain.AttemptRunning, nil
	case runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_WAITING_TOOL:
		return domain.AttemptWaitingTool, nil
	case runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_WAITING_AGENT:
		return domain.AttemptWaitingAgent, nil
	case runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_WAITING_APPROVAL:
		return domain.AttemptWaitingApproval, nil
	case runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_CHECKPOINTING:
		return domain.AttemptCheckpointing, nil
	case runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_FAILED:
		return domain.AttemptFailed, nil
	case runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_CANCEL_REQUESTED:
		return domain.AttemptCancelRequested, nil
	case runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_CANCELLED:
		return domain.AttemptCancelled, nil
	default:
		return "", fmt.Errorf("unsupported attempt phase %s", phase)
	}
}

func protoPhase(phase domain.AttemptPhase) runtimev1alpha1.AttemptPhase {
	switch phase {
	case domain.AttemptStarting:
		return runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_STARTING
	case domain.AttemptRunning:
		return runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_RUNNING
	case domain.AttemptWaitingTool:
		return runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_WAITING_TOOL
	case domain.AttemptWaitingAgent:
		return runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_WAITING_AGENT
	case domain.AttemptWaitingApproval:
		return runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_WAITING_APPROVAL
	case domain.AttemptCheckpointing:
		return runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_CHECKPOINTING
	case domain.AttemptFailed:
		return runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_FAILED
	case domain.AttemptCancelRequested:
		return runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_CANCEL_REQUESTED
	case domain.AttemptCancelled:
		return runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_CANCELLED
	default:
		return runtimev1alpha1.AttemptPhase_ATTEMPT_PHASE_UNSPECIFIED
	}
}

func rpcError(err error) error {
	if store.IsRetryableTransaction(err) {
		// Transient serialization failure: the caller must retry with
		// bounded backoff rather than treating the operation as failed.
		return status.Error(codes.Unavailable, "transient transaction conflict; retry")
	}
	switch {
	case errors.Is(err, store.ErrNoAssignment), errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, store.ErrFenced):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, store.ErrVersionConflict), errors.Is(err, store.ErrIdempotencyConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, store.ErrInvalidTransition), errors.Is(err, store.ErrLeaseNotExpired):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, "runtime control operation failed")
	}
}
