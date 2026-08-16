// Package gateway implements the fenced Tool Gateway boundary that runtime
// workers call on behalf of an Attempt. The gateway enforces policy, approval
// binding, budget hard-stop and durable side-effect receipts outside the LLM;
// it is a separate process in production (tech baseline §5) because it sits on
// the credential and external-side-effect security boundary.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	gatewayv1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/gateway/v1alpha1"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/bian-cloud-skill/agentos/internal/kernel/tool"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxArgsBytes = 64 << 10

// ToolInvoker is the decision chain the service exposes. The tool.Gateway
// satisfies it; tests substitute fakes.
type ToolInvoker interface {
	InvokeTool(context.Context, tool.InvokeInput) (tool.InvokeResult, error)
	ListTools(context.Context, string) ([]store.ToolDescriptor, error)
}

// Service is the fenced gRPC surface of the Tool Gateway. Development mode
// accepts a fixed tenant on a loopback-only listener; production exposure
// requires SPIFFE mTLS (ADR-006).
type Service struct {
	gatewayv1alpha1.UnimplementedToolGatewayServiceServer
	invoker       ToolInvoker
	allowedTenant string
}

func NewService(invoker ToolInvoker, allowedTenant string) *Service {
	return &Service{invoker: invoker, allowedTenant: allowedTenant}
}

func (s *Service) ListTools(ctx context.Context, request *gatewayv1alpha1.ListToolsRequest) (*gatewayv1alpha1.ListToolsResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if err := s.authorizeTenant(request.GetTenantId()); err != nil {
		return nil, err
	}
	descriptors, err := s.invoker.ListTools(ctx, request.GetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	response := &gatewayv1alpha1.ListToolsResponse{Tools: make([]*gatewayv1alpha1.ToolDescriptor, 0, len(descriptors))}
	for _, descriptor := range descriptors {
		response.Tools = append(response.Tools, &gatewayv1alpha1.ToolDescriptor{
			Name: descriptor.Name, Version: descriptor.Version,
			SideEffectRisk: string(descriptor.SideEffectRisk),
			Actions:        descriptor.Actions, ResourcePatterns: descriptor.ResourcePatterns,
			ParamsSchemaJson: descriptor.ParamsSchema, SpecDigest: fmt.Sprintf("%x", descriptor.SpecHash),
		})
	}
	return response, nil
}

func (s *Service) InvokeTool(ctx context.Context, request *gatewayv1alpha1.InvokeToolRequest) (*gatewayv1alpha1.InvokeToolResponse, error) {
	if request == nil || request.GetIdentity() == nil || request.GetIdentity().GetFencingToken() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "attempt identity and positive fencing token are required")
	}
	if err := s.authorizeTenant(request.GetIdentity().GetTenantId()); err != nil {
		return nil, err
	}
	if len(request.GetArgsJson()) > maxArgsBytes {
		return nil, status.Errorf(codes.InvalidArgument, "tool arguments exceed %d bytes", maxArgsBytes)
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
	input := tool.InvokeInput{
		TenantID: request.GetIdentity().GetTenantId(),
		TaskID:   taskID, RunID: runID, AttemptID: attemptID,
		FencingToken:    request.GetIdentity().GetFencingToken(),
		AgentVersionRef: request.GetAgentVersionRef(),
		ToolName:        request.GetToolName(), ToolVersion: request.GetToolVersion(),
		Action: request.GetAction(), Resource: request.GetResource(),
		Args: json.RawMessage(request.GetArgsJson()), IdempotencyKey: request.GetIdempotencyKey(),
	}
	if approvalID := request.GetApprovalId(); approvalID != "" {
		parsed, err := uuid.Parse(approvalID)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "approval ID must be a UUID")
		}
		input.ApprovalID = &parsed
	}
	result, err := s.invoker.InvokeTool(ctx, input)
	if err != nil {
		return nil, rpcError(err)
	}
	response := &gatewayv1alpha1.InvokeToolResponse{
		Outcome: string(result.Outcome), ResultJson: result.Result,
		DenyReasons: result.DenyReasons, PolicyRevision: result.PolicyRevision,
		ReceiptOperation: result.ReceiptOperation,
	}
	if result.ToolCall.ID != uuid.Nil {
		response.ToolCallId = result.ToolCall.ID.String()
	}
	if result.ApprovalID != nil {
		response.ApprovalId = result.ApprovalID.String()
	}
	return response, nil
}

func parseUUID(value, field string) (uuid.UUID, error) {
	if strings.TrimSpace(value) == "" {
		return uuid.Nil, status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, status.Errorf(codes.InvalidArgument, "%s must be a UUID", field)
	}
	return parsed, nil
}

func (s *Service) authorizeTenant(tenantID string) error {
	if strings.TrimSpace(tenantID) == "" || tenantID != s.allowedTenant {
		return status.Error(codes.PermissionDenied, "tenant is not authorized by this gateway endpoint")
	}
	return nil
}

func rpcError(err error) error {
	if store.IsRetryableTransaction(err) {
		// Transient serialization failure: the caller must retry with
		// bounded backoff rather than treating the operation as failed.
		return status.Error(codes.Unavailable, "transient transaction conflict; retry")
	}
	var approvalErr *tool.ApprovalNotUsableError
	if errors.As(err, &approvalErr) {
		// Pending parks the attempt; rejected, expired and mismatched
		// approvals can never authorize and must fail it.
		if approvalErr.Reason == tool.ApprovalPending {
			return status.Error(codes.FailedPrecondition, err.Error())
		}
		return status.Error(codes.PermissionDenied, err.Error())
	}
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrToolNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, store.ErrIdempotencyConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, store.ErrApprovalNotUsable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, tool.ErrBudgetExhausted):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, tool.ErrToolExecutionFailed):
		var executionErr *tool.ToolExecutionError
		if errors.As(err, &executionErr) {
			return status.Error(codes.Aborted, executionErr.Code)
		}
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, tool.ErrToolArgsInvalid), errors.Is(err, store.ErrInvalidTransition):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, "tool gateway operation failed")
	}
}
