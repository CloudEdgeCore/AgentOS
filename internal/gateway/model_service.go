package gateway

import (
	"context"
	"errors"
	"strings"

	modelv1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/model/v1alpha1"
	"github.com/bian-cloud-skill/agentos/internal/kernel/capability"
	"github.com/bian-cloud-skill/agentos/internal/kernel/model"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ModelInvoker is the fenced model decision chain the service exposes. The
// model.Gateway satisfies it.
type ModelInvoker interface {
	Begin(context.Context, model.BeginInput) (model.BeginResult, error)
	GetModelCall(context.Context, string, uuid.UUID) (store.ModelCall, error)
	Settle(context.Context, store.ModelCall, int64, model.Usage) error
	Finish(context.Context, store.ModelCall, model.FinishInput) (store.ModelCall, error)
}

// ModelService is the fenced gRPC surface of the Model Gateway. Development
// mode accepts a fixed tenant on a loopback-only listener; production
// exposure requires SPIFFE mTLS.
type ModelService struct {
	modelv1alpha1.UnimplementedModelGatewayServiceServer
	invoker       ModelInvoker
	allowedTenant string
	capabilities  *capability.Authorizer
}

func NewModelService(invoker ModelInvoker, allowedTenant string, capabilities ...*capability.Authorizer) *ModelService {
	service := &ModelService{invoker: invoker, allowedTenant: allowedTenant}
	if len(capabilities) > 0 {
		service.capabilities = capabilities[0]
	}
	return service
}

func (s *ModelService) Begin(ctx context.Context, request *modelv1alpha1.BeginRequest) (*modelv1alpha1.BeginResponse, error) {
	if request == nil || request.GetIdentity() == nil || request.GetIdentity().GetFencingToken() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "attempt identity and positive fencing token are required")
	}
	if err := s.authorizeTenant(request.GetIdentity().GetTenantId()); err != nil {
		return nil, err
	}
	if s.capabilities != nil {
		if err := s.capabilities.Authorize(ctx, request.GetIdentity().GetTenantId(), request.GetAgentVersionRef(),
			capability.Model, request.GetModelRef()); err != nil {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
	}
	taskID, err := parseUUID(request.GetTaskId(), "task ID")
	if err != nil {
		return nil, err
	}
	runID, err := parseUUID(request.GetRunId(), "run ID")
	if err != nil {
		return nil, err
	}
	attemptID, err := parseUUID(request.GetIdentity().GetAttemptId(), "attempt ID")
	if err != nil {
		return nil, err
	}
	result, err := s.invoker.Begin(ctx, model.BeginInput{
		TenantID: request.GetIdentity().GetTenantId(), TaskID: taskID, RunID: runID, AttemptID: attemptID,
		FencingToken:    request.GetIdentity().GetFencingToken(),
		AgentVersionRef: request.GetAgentVersionRef(), ModelRef: request.GetModelRef(),
		IdempotencyKey: request.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, modelRPCError(err)
	}
	return &modelv1alpha1.BeginResponse{
		CallId: result.Call.ID.String(), ModelRef: result.Call.ModelRef,
		Status: string(result.Call.Status), PriceRevision: result.Call.PriceRevision,
		PolicyRevision: result.PolicyRevision, ResourceVersion: result.Call.ResourceVersion,
	}, nil
}

func (s *ModelService) Settle(ctx context.Context, request *modelv1alpha1.SettleRequest) (*modelv1alpha1.SettleResponse, error) {
	if request == nil || request.GetIdentity() == nil || request.GetIdentity().GetFencingToken() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "attempt identity and positive fencing token are required")
	}
	if err := s.authorizeTenant(request.GetIdentity().GetTenantId()); err != nil {
		return nil, err
	}
	if request.GetSequence() < 1 {
		return nil, status.Error(codes.InvalidArgument, "settlement sequence must be positive")
	}
	callID, err := parseUUID(request.GetCallId(), "call ID")
	if err != nil {
		return nil, err
	}
	call, err := s.invoker.GetModelCall(ctx, request.GetIdentity().GetTenantId(), callID)
	if err != nil {
		return nil, modelRPCError(err)
	}
	if err := s.invoker.Settle(ctx, call, request.GetSequence(), model.Usage{
		InputTokens: request.GetInputTokens(), OutputTokens: request.GetOutputTokens(),
	}); err != nil {
		return nil, modelRPCError(err)
	}
	return &modelv1alpha1.SettleResponse{CallId: callID.String()}, nil
}

func (s *ModelService) Finish(ctx context.Context, request *modelv1alpha1.FinishRequest) (*modelv1alpha1.FinishResponse, error) {
	if request == nil || request.GetIdentity() == nil || request.GetIdentity().GetFencingToken() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "attempt identity and positive fencing token are required")
	}
	if err := s.authorizeTenant(request.GetIdentity().GetTenantId()); err != nil {
		return nil, err
	}
	callID, err := parseUUID(request.GetCallId(), "call ID")
	if err != nil {
		return nil, err
	}
	call, err := s.invoker.GetModelCall(ctx, request.GetIdentity().GetTenantId(), callID)
	if err != nil {
		return nil, modelRPCError(err)
	}
	finished, err := s.invoker.Finish(ctx, call, model.FinishInput{
		TenantID: call.TenantID, ModelCallID: call.ID, ExpectedVersion: request.GetExpectedVersion(),
		Status: store.ModelCallStatus(request.GetStatus()), InputTokens: request.GetInputTokens(),
		OutputTokens: request.GetOutputTokens(), ProviderRequestID: request.GetProviderRequestId(),
		FinishReason: request.GetFinishReason(),
	})
	if err != nil {
		return nil, modelRPCError(err)
	}
	return &modelv1alpha1.FinishResponse{
		CallId: finished.ID.String(), ModelRef: finished.ModelRef, Status: string(finished.Status),
		InputTokens: finished.InputTokens, OutputTokens: finished.OutputTokens,
		CostUsd: finished.CostUSD, PriceRevision: finished.PriceRevision, FinishReason: finished.FinishReason,
	}, nil
}

func (s *ModelService) authorizeTenant(tenantID string) error {
	if strings.TrimSpace(tenantID) == "" || tenantID != s.allowedTenant {
		return status.Error(codes.PermissionDenied, "tenant is not authorized by this gateway endpoint")
	}
	return nil
}

func modelRPCError(err error) error {
	if store.IsRetryableTransaction(err) {
		return status.Error(codes.Unavailable, "transient transaction conflict; retry")
	}
	switch {
	case errors.Is(err, model.ErrModelDenied):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, model.ErrBudgetExhausted), errors.Is(err, store.ErrBudgetExceeded):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrModelNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, store.ErrVersionConflict), errors.Is(err, store.ErrIdempotencyConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, store.ErrInvalidTransition):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, "model gateway operation failed")
	}
}
