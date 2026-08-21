package reference

import (
	"context"
	"encoding/json"
	"fmt"

	gatewayv1alpha1 "github.com/bian-cloud-skill/agentos/gen/go/agentos/gateway/v1alpha1"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/bian-cloud-skill/agentos/internal/kernel/tool"
	"github.com/bian-cloud-skill/agentos/internal/mcp"
	"github.com/google/uuid"
)

// GrpcToolInvoker forwards MCP tool calls over the fenced Tool Gateway gRPC
// boundary, preserving the attempt identity (tenant, attempt, fencing token)
// so the gateway's decision chain is exercised exactly like scripted calls.
type GrpcToolInvoker struct {
	client gatewayv1alpha1.ToolGatewayServiceClient
}

func NewGrpcToolInvoker(client gatewayv1alpha1.ToolGatewayServiceClient) *GrpcToolInvoker {
	return &GrpcToolInvoker{client: client}
}

var _ mcp.ToolInvoker = (*GrpcToolInvoker)(nil)

func (g *GrpcToolInvoker) ListTools(ctx context.Context, tenantID string) ([]store.ToolDescriptor, error) {
	response, err := g.client.ListTools(ctx, &gatewayv1alpha1.ListToolsRequest{TenantId: tenantID})
	if err != nil {
		return nil, fmt.Errorf("list tools over Runtime Protocol: %w", err)
	}
	descriptors := make([]store.ToolDescriptor, 0, len(response.GetTools()))
	for _, proto := range response.GetTools() {
		descriptors = append(descriptors, store.ToolDescriptor{
			Name: proto.GetName(), Version: proto.GetVersion(),
			SideEffectRisk: store.ToolRisk(proto.GetSideEffectRisk()),
			Actions:        proto.GetActions(), ResourcePatterns: proto.GetResourcePatterns(),
			ParamsSchema: json.RawMessage(proto.GetParamsSchemaJson()),
		})
	}
	return descriptors, nil
}

func (g *GrpcToolInvoker) InvokeTool(ctx context.Context, in tool.InvokeInput) (tool.InvokeResult, error) {
	request := &gatewayv1alpha1.InvokeToolRequest{
		Identity: &gatewayv1alpha1.AttemptIdentity{
			TenantId: in.TenantID, AttemptId: in.AttemptID.String(), FencingToken: in.FencingToken,
		},
		TaskId: in.TaskID.String(), RunId: in.RunID.String(),
		AgentVersionRef: in.AgentVersionRef, ToolName: in.ToolName, ToolVersion: in.ToolVersion,
		Action: in.Action, Resource: in.Resource, ArgsJson: in.Args, IdempotencyKey: in.IdempotencyKey,
	}
	if in.ApprovalID != nil {
		request.ApprovalId = in.ApprovalID.String()
	}
	response, err := g.client.InvokeTool(ctx, request)
	if err != nil {
		return tool.InvokeResult{}, err
	}
	result := tool.InvokeResult{
		Outcome: tool.InvokeOutcome(response.GetOutcome()),
		Result:  response.GetResultJson(), DenyReasons: response.GetDenyReasons(),
		PolicyRevision: response.GetPolicyRevision(), ReceiptOperation: response.GetReceiptOperation(),
	}
	if callID := response.GetToolCallId(); callID != "" {
		if parsed, parseErr := uuid.Parse(callID); parseErr == nil {
			result.ToolCall.ID = parsed
		}
	}
	if approvalID := response.GetApprovalId(); approvalID != "" {
		if parsed, parseErr := uuid.Parse(approvalID); parseErr == nil {
			result.ApprovalID = &parsed
		}
	}
	return result, nil
}
