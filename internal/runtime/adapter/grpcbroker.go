// gRPC broker clients: the adapter worker's loopback MCP endpoint reaches
// the Model execution layer and the Memory store through the same fenced
// gateway services every other runtime uses.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"

	gatewayv1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/gateway/v1"
	modelv1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/model/v1"
	runtimev1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/runtime/v1"
	kernelmodel "github.com/CloudEdgeCore/AgentOS/internal/kernel/memory"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/model"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/model/provider"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/money"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/CloudEdgeCore/AgentOS/internal/mcp"
	"github.com/google/uuid"
)

// GrpcModelBroker implements mcp.ModelBroker against ModelInvocationService.
type GrpcModelBroker struct {
	client modelv1.ModelInvocationServiceClient
}

// NewGrpcModelBroker binds the broker to a gateway connection.
func NewGrpcModelBroker(client modelv1.ModelInvocationServiceClient) *GrpcModelBroker {
	return &GrpcModelBroker{client: client}
}

// InvokeStream streams one invocation, forwarding deltas and mapping the
// terminal chunk onto the kernel output shape.
func (b *GrpcModelBroker) InvokeStream(ctx context.Context, in model.InvokeInput, onDelta func(string)) (model.InvokeOutput, error) {
	messages := make([]*modelv1.ChatMessage, 0, len(in.Messages))
	for _, message := range in.Messages {
		entry := &modelv1.ChatMessage{Role: message.Role, Content: message.Content, ToolCallId: message.ToolCallID}
		for _, call := range message.ToolCalls {
			entry.ToolCalls = append(entry.ToolCalls, &modelv1.ChatToolCall{
				Id: call.ID, Name: call.Name, ArgumentsJson: call.Arguments,
			})
		}
		messages = append(messages, entry)
	}
	stream, err := b.client.Invoke(ctx, &modelv1.InvokeRequest{
		Identity: &modelv1.AttemptIdentity{
			TenantId: in.TenantID, AttemptId: in.AttemptID.String(), FencingToken: in.FencingToken,
		},
		TaskId: in.TaskID.String(), RunId: in.RunID.String(), AgentVersionRef: in.AgentVersionRef,
		ModelRef: in.ModelRef, IdempotencyKey: in.IdempotencyKey, Messages: messages,
		Stream: in.Stream, MaxOutputTokens: in.MaxOutputTokens, Temperature: in.Temperature,
	})
	if err != nil {
		return model.InvokeOutput{}, fmt.Errorf("open model invocation: %w", err)
	}
	output := model.InvokeOutput{}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return output, nil
		}
		if err != nil {
			return output, fmt.Errorf("model invocation stream: %w", err)
		}
		switch {
		case chunk.GetDelta() != "":
			if onDelta != nil {
				onDelta(chunk.GetDelta())
			}
		case chunk.GetToolCall() != nil:
			output.ToolCalls = append(output.ToolCalls, provider.ToolCall{
				ID: chunk.GetToolCall().GetId(), Name: chunk.GetToolCall().GetName(),
				Arguments: chunk.GetToolCall().GetArgumentsJson(),
			})
		case chunk.GetFinish() != nil:
			finish := chunk.GetFinish()
			callID, parseErr := uuid.Parse(finish.GetCallId())
			if parseErr != nil {
				return output, fmt.Errorf("finish chunk call id: %w", parseErr)
			}
			cost, parseErr := money.FromUSD(finish.GetCostUsd())
			if parseErr != nil {
				return output, fmt.Errorf("finish chunk cost: %w", parseErr)
			}
			output.Content = finish.GetContent()
			output.Call = store.ModelCall{
				ID: callID, TenantID: in.TenantID, TaskID: in.TaskID, RunID: in.RunID, AttemptID: in.AttemptID,
				ModelRef: finish.GetModelRef(), Status: store.ModelCallStatus(finish.GetStatus()),
				InputTokens: finish.GetInputTokens(), OutputTokens: finish.GetOutputTokens(),
				CostMicroUSD: cost, PriceRevision: finish.GetPriceRevision(),
				ProviderRequestID: finish.GetProviderRequestId(), FinishReason: finish.GetFinishReason(),
			}
		case chunk.GetFailure() != nil:
			failure := chunk.GetFailure()
			output.Call.Status = store.ModelCallFailed
			output.Call.FinishReason = failure.GetFinishReason()
			return output, fmt.Errorf("model invocation failed: %s: %s", failure.GetCode(), failure.GetMessage())
		}
	}
}

// GrpcMemoryBroker implements mcp.MemoryBroker against MemoryGatewayService.
type GrpcMemoryBroker struct {
	client gatewayv1.MemoryGatewayServiceClient
}

// NewGrpcMemoryBroker binds the broker to a gateway connection.
func NewGrpcMemoryBroker(client gatewayv1.MemoryGatewayServiceClient) *GrpcMemoryBroker {
	return &GrpcMemoryBroker{client: client}
}

// Put writes one record with the fenced identity of the calling attempt.
func (b *GrpcMemoryBroker) Put(ctx context.Context, identity mcp.AttemptContext, in kernelmodel.PutInput) (store.MemoryRecord, bool, error) {
	put, err := b.client.PutMemory(ctx, &gatewayv1.PutMemoryRequest{
		Identity:        &gatewayv1.AttemptIdentity{TenantId: identity.TenantID, AttemptId: identity.AttemptID.String(), FencingToken: identity.FencingToken},
		AgentVersionRef: identity.AgentVersionRef,
		Namespace:       in.Namespace, Key: in.Key, ContentType: in.ContentType,
		Content: in.Content, Sensitivity: in.Sensitivity,
	})
	if err != nil {
		return store.MemoryRecord{}, false, fmt.Errorf("memory put: %w", err)
	}
	return memoryRecordFromProto(put.GetRecord()), put.GetReplayed(), nil
}

// Search retrieves records with the fenced identity of the calling attempt.
func (b *GrpcMemoryBroker) Search(ctx context.Context, identity mcp.AttemptContext, in kernelmodel.SearchInput) ([]store.MemoryRecord, error) {
	found, err := b.client.SearchMemory(ctx, &gatewayv1.SearchMemoryRequest{
		Identity:        &gatewayv1.AttemptIdentity{TenantId: identity.TenantID, AttemptId: identity.AttemptID.String(), FencingToken: identity.FencingToken},
		AgentVersionRef: identity.AgentVersionRef,
		Namespace:       in.Namespace, Query: in.Query, Sensitivity: in.Sensitivity, Limit: int32(in.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("memory search: %w", err)
	}
	records := make([]store.MemoryRecord, 0, len(found.GetRecords()))
	for _, record := range found.GetRecords() {
		records = append(records, memoryRecordFromProto(record))
	}
	return records, nil
}

func memoryRecordFromProto(record *gatewayv1.MemoryRecord) store.MemoryRecord {
	converted := store.MemoryRecord{
		Namespace: record.GetNamespace(), Key: record.GetKey(), ContentType: record.GetContentType(),
		Content: record.GetContent(), Sensitivity: record.GetSensitivity(), ResourceVersion: record.GetResourceVersion(),
	}
	if id, err := uuid.Parse(record.GetId()); err == nil {
		converted.ID = id
	}
	if record.GetCreatedAt() != nil {
		converted.CreatedAt = record.GetCreatedAt().AsTime()
	}
	if record.GetUpdatedAt() != nil {
		converted.UpdatedAt = record.GetUpdatedAt().AsTime()
	}
	return converted
}

// GrpcWorkflowSpawner implements mcp.WorkflowSpawner against the
// orchestrator's WorkflowSpawnService (v1.3).
type GrpcWorkflowSpawner struct {
	client runtimev1.WorkflowSpawnServiceClient
}

// NewGrpcWorkflowSpawner binds the spawner to an orchestrator connection.
func NewGrpcWorkflowSpawner(client runtimev1.WorkflowSpawnServiceClient) *GrpcWorkflowSpawner {
	return &GrpcWorkflowSpawner{client: client}
}

// Spawn forwards one dynamic-step spawn; guard denials arrive as structured
// outcomes, not gRPC errors.
func (s *GrpcWorkflowSpawner) Spawn(ctx context.Context, in mcp.SpawnRequest) (mcp.SpawnOutcome, error) {
	response, err := s.client.SpawnStep(ctx, &runtimev1.SpawnStepRequest{
		Identity: &runtimev1.AttemptIdentity{
			TenantId: in.TenantID, AttemptId: in.AttemptID.String(), FencingToken: in.FencingToken,
		},
		WorkflowId: in.WorkflowID.String(),
		ParentStep: in.ParentStepName, Name: in.Name, Goal: in.Goal,
		AgentVersionRef: in.AgentVersionRef, SpecJson: string(in.Spec),
		MaxAttempts: int32(in.MaxAttempts), IdempotencyKey: in.IdempotencyKey,
		ArgumentsJson: string(in.Arguments),
	})
	if err != nil {
		return mcp.SpawnOutcome{}, fmt.Errorf("spawn step: %w", err)
	}
	return mcp.SpawnOutcome{
		Code: response.GetOutcome(), Message: response.GetMessage(),
		StepName: response.GetStepName(), SpawnDepth: int(response.GetSpawnDepth()),
	}, nil
}
