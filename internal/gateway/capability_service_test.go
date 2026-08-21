package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	gatewayv1 "github.com/CloudEdgeCore/AgentOS/gen/go/agentos/gateway/v1"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/agentversion"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/capability"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/memory"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/tool"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestToolServiceEnforcesSecretCapability(t *testing.T) {
	authorizer := testAuthorizer(t, agentversion.Capabilities{
		Tools: []string{"fs.read"}, Models: []string{}, Memory: []string{}, Secrets: []string{"database/read"},
	})
	invoker := &fakeInvoker{result: tool.InvokeResult{Outcome: tool.OutcomeExecuted}}
	service := NewService(invoker, "tenant-a", authorizer)

	denied := invokeRequest(t, "")
	denied.SecretRef = "database/admin"
	if _, err := service.InvokeTool(context.Background(), denied); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("undeclared secret: %v, want PermissionDenied", err)
	}
	if invoker.input.ToolName != "" {
		t.Fatal("denied secret reached the tool decision chain")
	}

	allowed := invokeRequest(t, "")
	allowed.SecretRef = "database/read"
	if _, err := service.InvokeTool(context.Background(), allowed); err != nil {
		t.Fatalf("declared secret: %v", err)
	}
	if invoker.input.SecretRef != "database/read" {
		t.Fatalf("secret grant not propagated: %+v", invoker.input)
	}
}

func TestMemoryServiceFencesAndAuthorizesReadWrite(t *testing.T) {
	authorizer := testAuthorizer(t, agentversion.Capabilities{
		Tools: []string{}, Models: []string{}, Memory: []string{"project:read", "project:write"}, Secrets: []string{},
	})
	taskID, runID := uuid.New(), uuid.New()
	fence := &fakeMemoryFence{assignment: store.RuntimeAssignment{
		Task: store.Task{ID: taskID, TenantID: "tenant-a", AgentVersionRef: "agent@1"},
		Run:  store.Run{ID: runID}, Attempt: store.Attempt{ID: attemptID},
	}}
	memories := &fakeMemoryInvoker{}
	service := NewMemoryService(memories, fence, "tenant-a", authorizer)
	identity := &gatewayv1.AttemptIdentity{TenantId: "tenant-a", AttemptId: attemptID.String(), FencingToken: 7}

	put, err := service.PutMemory(context.Background(), &gatewayv1.PutMemoryRequest{
		Identity: identity, AgentVersionRef: "agent@1", Namespace: "project", Key: "status",
		ContentType: "text/plain", Content: "ready", Sensitivity: "internal", ProvenanceJson: []byte(`{"source":"test"}`),
	})
	if err != nil {
		t.Fatalf("PutMemory: %v", err)
	}
	if put.GetRecord().GetContent() != "ready" || memories.put.SourceAttemptID == nil || *memories.put.SourceAttemptID != attemptID {
		t.Fatalf("write was not provenance-bound: response=%+v input=%+v", put, memories.put)
	}

	search, err := service.SearchMemory(context.Background(), &gatewayv1.SearchMemoryRequest{
		Identity: identity, AgentVersionRef: "agent@1", Namespace: "project", Query: "ready", Limit: 5,
	})
	if err != nil || len(search.GetRecords()) != 1 {
		t.Fatalf("SearchMemory: response=%+v error=%v", search, err)
	}

	_, err = service.SearchMemory(context.Background(), &gatewayv1.SearchMemoryRequest{
		Identity: identity, AgentVersionRef: "agent@1", Namespace: "private", Query: "ready", Limit: 5,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("undeclared namespace: %v, want PermissionDenied", err)
	}
	if memories.searchCalls != 1 {
		t.Fatal("denied namespace reached canonical memory")
	}
}

func TestMemoryServiceRejectsStaleAndMismatchedAttempt(t *testing.T) {
	authorizer := testAuthorizer(t, agentversion.Capabilities{
		Tools: []string{}, Models: []string{}, Memory: []string{"*"}, Secrets: []string{},
	})
	identity := &gatewayv1.AttemptIdentity{TenantId: "tenant-a", AttemptId: attemptID.String(), FencingToken: 1}
	service := NewMemoryService(&fakeMemoryInvoker{}, &fakeMemoryFence{err: store.ErrFenced}, "tenant-a", authorizer)
	_, err := service.SearchMemory(context.Background(), &gatewayv1.SearchMemoryRequest{
		Identity: identity, AgentVersionRef: "agent@1", Namespace: "project", Query: "x", Limit: 1,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stale fence: %v", err)
	}

	service = NewMemoryService(&fakeMemoryInvoker{}, &fakeMemoryFence{assignment: store.RuntimeAssignment{
		Task: store.Task{AgentVersionRef: "other@1"}, Attempt: store.Attempt{ID: attemptID},
	}}, "tenant-a", authorizer)
	_, err = service.SearchMemory(context.Background(), &gatewayv1.SearchMemoryRequest{
		Identity: identity, AgentVersionRef: "agent@1", Namespace: "project", Query: "x", Limit: 1,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("mismatched publication: %v", err)
	}
}

type gatewayVersionStore struct{ version store.AgentVersion }

func (s gatewayVersionStore) CreateAgentVersion(context.Context, store.CreateAgentVersionInput) (store.CreateAgentVersionResult, error) {
	return store.CreateAgentVersionResult{}, errors.New("not implemented")
}
func (s gatewayVersionStore) GetAgentVersion(context.Context, string, uuid.UUID) (store.AgentVersion, error) {
	return store.AgentVersion{}, errors.New("not implemented")
}
func (s gatewayVersionStore) GetAgentVersionByRef(_ context.Context, tenant, ref string) (store.AgentVersion, error) {
	if tenant != s.version.TenantID || ref != s.version.Ref() {
		return store.AgentVersion{}, store.ErrNotFound
	}
	return s.version, nil
}

func testAuthorizer(t *testing.T, capabilities agentversion.Capabilities) *capability.Authorizer {
	t.Helper()
	spec, err := json.Marshal(agentversion.Spec{
		Runtimes: []agentversion.RuntimeTarget{{Class: "remote"}}, Capabilities: &capabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := capability.NewAuthorizer(gatewayVersionStore{version: store.AgentVersion{
		TenantID: "tenant-a", Name: "agent", Version: "1", Spec: spec,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return authorizer
}

type fakeMemoryFence struct {
	assignment store.RuntimeAssignment
	err        error
}

func (f *fakeMemoryFence) GetRuntimeAssignment(context.Context, string, uuid.UUID, int64) (store.RuntimeAssignment, error) {
	return f.assignment, f.err
}

type fakeMemoryInvoker struct {
	put         memory.PutInput
	searchCalls int
}

func (f *fakeMemoryInvoker) Put(_ context.Context, input memory.PutInput) (store.MemoryRecord, bool, error) {
	f.put = input
	now := time.Now()
	return store.MemoryRecord{
		ID: uuid.New(), TenantID: input.TenantID, Namespace: input.Namespace, Key: input.Key,
		ContentType: input.ContentType, Content: input.Content, Sensitivity: input.Sensitivity,
		ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
	}, false, nil
}

func (f *fakeMemoryInvoker) Search(_ context.Context, input memory.SearchInput) ([]store.MemoryRecord, error) {
	f.searchCalls++
	now := time.Now()
	return []store.MemoryRecord{{
		ID: uuid.New(), TenantID: input.TenantID, Namespace: input.Namespace, Key: "status",
		ContentType: "text/plain", Content: "ready", Sensitivity: "internal", ResourceVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}}, nil
}
