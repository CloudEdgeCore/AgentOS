package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	controlapi "github.com/bian-cloud-skill/agentos/internal/control/api"
	"github.com/bian-cloud-skill/agentos/internal/control/auth"
	"github.com/bian-cloud-skill/agentos/internal/kernel/agentversion"
	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

func TestCreateTaskRequiresIdentityAndIdempotency(t *testing.T) {
	handler := controlapi.NewHandler(newMemoryStore(), newMemoryStore(), newMemoryStore())
	body := []byte(`{"agentVersionRef":"agent@1","goal":"test","namespace":"default","spec":{}}`)

	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "request-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", response.Code, response.Body.String())
	}

	authed := auth.StaticMiddleware(auth.Principal{Subject: "user-1", TenantID: "tenant-a"}, handler)
	request = httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	authed.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
	assertReason(t, response, "INVALID_IDEMPOTENCY_KEY")
}

func TestCreateTaskIsStrictAndIdempotent(t *testing.T) {
	backend := newMemoryStore()
	handler := auth.StaticMiddleware(
		auth.Principal{Subject: "user-1", TenantID: "tenant-a"},
		controlapi.NewHandler(backend, backend, backend),
	)
	body := []byte(`{"agentVersionRef":"agent@1","goal":"test","namespace":"default","spec":{"runtimeClass":"oci"}}`)

	first := performCreate(handler, body, "request-1")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d: %s", first.Code, first.Body.String())
	}
	var task map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if task["tenantId"] != "tenant-a" || task["phase"] != "QUEUED" {
		t.Fatalf("unexpected task response: %+v", task)
	}
	if first.Header().Get("Location") == "" || first.Header().Get("ETag") != `W/"1"` {
		t.Fatalf("missing resource headers: %+v", first.Header())
	}

	replayed := performCreate(handler, body, "request-1")
	if replayed.Code != http.StatusOK || replayed.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", replayed.Code, replayed.Header(), replayed.Body.String())
	}

	conflict := performCreate(handler, []byte(`{"agentVersionRef":"agent@1","goal":"different","namespace":"default","spec":{}}`), "request-1")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d: %s", conflict.Code, conflict.Body.String())
	}
	assertReason(t, conflict, "IDEMPOTENCY_CONFLICT")

	unknown := performCreate(handler, []byte(`{"agentVersionRef":"agent@1","goal":"test","namespace":"default","spec":{},"status":"SUCCEEDED"}`), "request-2")
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d: %s", unknown.Code, unknown.Body.String())
	}
	assertReason(t, unknown, "INVALID_JSON")

	duplicate := performCreate(handler, []byte(`{"agentVersionRef":"agent@1","goal":"first","goal":"second","namespace":"default","spec":{"budget":{"tokens":1,"tokens":2}}}`), "request-3")
	if duplicate.Code != http.StatusBadRequest {
		t.Fatalf("duplicate key status = %d: %s", duplicate.Code, duplicate.Body.String())
	}
	assertReason(t, duplicate, "INVALID_JSON")

	malformed := performCreate(handler, []byte(`{"agentVersionRef":"not-a-ref","goal":"test","namespace":"default","spec":{}}`), "request-4")
	if malformed.Code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed ref status = %d: %s", malformed.Code, malformed.Body.String())
	}
	assertReason(t, malformed, "INVALID_TASK")
}

func TestGetTaskDoesNotCrossTenantBoundary(t *testing.T) {
	backend := newMemoryStore()
	task, err := backend.CreateTask(context.Background(), store.CreateTaskInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent@1",
		Goal: "private", Spec: []byte(`{}`), IdempotencyKey: "request-1",
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	handler := controlapi.NewHandler(backend, backend, backend)

	owner := auth.StaticMiddleware(auth.Principal{Subject: "owner", TenantID: "tenant-a"}, handler)
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.Task.ID.String(), nil)
	response := httptest.NewRecorder()
	owner.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("owner status = %d: %s", response.Code, response.Body.String())
	}

	other := auth.StaticMiddleware(auth.Principal{Subject: "other", TenantID: "tenant-b"}, handler)
	request = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.Task.ID.String(), nil)
	response = httptest.NewRecorder()
	other.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("other tenant status = %d, want 404: %s", response.Code, response.Body.String())
	}
}

func TestCancelTaskRequiresCurrentETag(t *testing.T) {
	backend := newMemoryStore()
	created, err := backend.CreateTask(context.Background(), store.CreateTaskInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent@1",
		Goal: "cancel me", Spec: []byte(`{}`), IdempotencyKey: "cancel-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := auth.StaticMiddleware(auth.Principal{Subject: "owner", TenantID: "tenant-a"}, controlapi.NewHandler(backend, backend, backend))
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+created.Task.ID.String()+":cancel", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/tasks/"+created.Task.ID.String()+":cancel", nil)
	request.Header.Set("If-Match", `W/"1"`)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("ETag") != `W/"2"` {
		t.Fatalf("cancel status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var body taskPhaseResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Phase != "CANCELLED" {
		t.Fatalf("cancel response=%+v err=%v", body, err)
	}
}

type taskPhaseResponse struct {
	Phase string `json:"phase"`
}

func TestPublishAgentVersionIsStrictAndIdempotent(t *testing.T) {
	backend := newMemoryStore()
	handler := auth.StaticMiddleware(
		auth.Principal{Subject: "user-1", TenantID: "tenant-a"},
		controlapi.NewHandler(backend, backend, backend),
	)
	spec := []byte(`{"runtimeClassPolicy":{"allowed":["oci"],"preferred":"oci"},"lifecycle":{"maxAttempts":5}}`)
	first := performPublish(handler, spec, "publish-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d: %s", first.Code, first.Body.String())
	}
	var published map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &published); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if published["tenantId"] != "tenant-a" || published["name"] != "research-agent" ||
		published["version"] != "1.3.0" || published["ref"] != "research-agent@1.3.0" {
		t.Fatalf("unexpected agent version response: %+v", published)
	}
	if first.Header().Get("Location") == "" || first.Header().Get("ETag") != `W/"1"` {
		t.Fatalf("missing resource headers: %+v", first.Header())
	}
	digest, ok := published["specDigest"].(string)
	if !ok || len(digest) != 64 {
		t.Fatalf("missing spec digest: %+v", published)
	}

	replayed := performPublish(handler, spec, "publish-2")
	if replayed.Code != http.StatusOK || replayed.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", replayed.Code, replayed.Header(), replayed.Body.String())
	}

	conflict := performPublish(handler, []byte(`{"runtimeClassPolicy":{"allowed":["microvm"]}}`), "publish-3")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d: %s", conflict.Code, conflict.Body.String())
	}
	assertReason(t, conflict, "AGENT_VERSION_CONFLICT")

	unknownBody := []byte(`{"name":"research-agent","version":"2.0.0","namespace":"default","spec":{"runtimeClassPolicy":{"allowed":["oci"]}},"status":"ACTIVE"}`)
	unknown := publishRequest(unknownBody, "publish-4")
	unknownResponse := httptest.NewRecorder()
	handler.ServeHTTP(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d: %s", unknownResponse.Code, unknownResponse.Body.String())
	}
	assertReason(t, unknownResponse, "INVALID_JSON")

	invalidSpec := performPublishVersion(handler, "research-agent", "2.1.0", []byte(`{"runtimeClassPolicy":{"allowed":["oci"],"preferred":"microvm"}}`), "publish-5")
	if invalidSpec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid spec status = %d: %s", invalidSpec.Code, invalidSpec.Body.String())
	}
	assertReason(t, invalidSpec, "INVALID_AGENT_VERSION")

	missingKey := publishRequest(spec, "")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, missingKey)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key status = %d: %s", response.Code, response.Body.String())
	}
	assertReason(t, response, "INVALID_IDEMPOTENCY_KEY")
}

func TestGetAgentVersionDoesNotCrossTenantBoundary(t *testing.T) {
	backend := newMemoryStore()
	published, err := backend.CreateAgentVersion(context.Background(), store.CreateAgentVersionInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", Name: "research-agent", Version: "1.3.0",
		Spec: []byte(`{"runtimeClassPolicy":{"allowed":["oci"]}}`),
	})
	if err != nil {
		t.Fatalf("seed agent version: %v", err)
	}
	handler := controlapi.NewHandler(backend, backend, backend)

	owner := auth.StaticMiddleware(auth.Principal{Subject: "owner", TenantID: "tenant-a"}, handler)
	request := httptest.NewRequest(http.MethodGet, "/v1/agent-versions/"+published.AgentVersion.ID.String(), nil)
	response := httptest.NewRecorder()
	owner.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("owner status = %d: %s", response.Code, response.Body.String())
	}

	other := auth.StaticMiddleware(auth.Principal{Subject: "other", TenantID: "tenant-b"}, handler)
	request = httptest.NewRequest(http.MethodGet, "/v1/agent-versions/"+published.AgentVersion.ID.String(), nil)
	response = httptest.NewRecorder()
	other.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("other tenant status = %d, want 404: %s", response.Code, response.Body.String())
	}
}

func performPublish(handler http.Handler, spec []byte, key string) *httptest.ResponseRecorder {
	return performPublishVersion(handler, "research-agent", "1.3.0", spec, key)
}

func performPublishVersion(handler http.Handler, name, version string, spec []byte, key string) *httptest.ResponseRecorder {
	body := []byte(`{"name":"` + name + `","version":"` + version + `","namespace":"default","spec":` + string(spec) + `}`)
	request := publishRequest(body, key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func publishRequest(body []byte, key string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/agent-versions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	return request
}

func TestGetTaskReportsBudgetUsage(t *testing.T) {
	backend := newMemoryStore()
	created, err := backend.CreateTask(context.Background(), store.CreateTaskInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent@1",
		Goal: "usage", Spec: []byte(`{}`), IdempotencyKey: "usage-task",
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	backend.setBudget(created.Task.ID, store.TaskBudgetStatus{
		TaskID: created.Task.ID, TenantID: "tenant-a",
		Reserved:  store.TaskBudget{Tokens: 100, CostUSD: 1, ToolCalls: 10, WallSeconds: 60},
		Consumed:  store.TaskBudget{Tokens: 60, CostUSD: 0.5, ToolCalls: 4, WallSeconds: 30},
		Exhausted: true, ResourceVersion: 2, UpdatedAt: now,
	})
	handler := auth.StaticMiddleware(auth.Principal{Subject: "owner", TenantID: "tenant-a"}, controlapi.NewHandler(backend, backend, backend))
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+created.Task.ID.String(), nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Usage           usageResponse `json:"usage"`
		BudgetExhausted bool          `json:"budgetExhausted"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Usage.Tokens != 60 || body.Usage.CostUSD != 0.5 || body.Usage.ToolCalls != 4 || body.Usage.WallSeconds != 30 {
		t.Fatalf("unexpected usage: %+v", body.Usage)
	}
	if !body.BudgetExhausted {
		t.Fatal("budgetExhausted = false, want true")
	}
}

type usageResponse struct {
	Tokens      int64   `json:"tokens"`
	CostUSD     float64 `json:"costUsd"`
	ToolCalls   int64   `json:"toolCalls"`
	WallSeconds int64   `json:"wallSeconds"`
}

func TestRoutingFailuresRemainStructured(t *testing.T) {
	handler := controlapi.NewHandler(newMemoryStore(), newMemoryStore(), newMemoryStore())
	request := httptest.NewRequest(http.MethodDelete, "/v1/tasks", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("method response status=%d content-type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	assertReason(t, response, "METHOD_NOT_ALLOWED")

	request = httptest.NewRequest(http.MethodGet, "/unknown", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("route response status=%d body=%s", response.Code, response.Body.String())
	}
	assertReason(t, response, "ROUTE_NOT_FOUND")
}

func performCreate(handler http.Handler, body []byte, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertReason(t *testing.T, response *httptest.ResponseRecorder, expected string) {
	t.Helper()
	var body struct {
		ReasonCode string `json:"reasonCode"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if body.ReasonCode != expected {
		t.Fatalf("reason = %q, want %q", body.ReasonCode, expected)
	}
}

type memoryStore struct {
	mu        sync.Mutex
	byID      map[uuid.UUID]store.Task
	byKey     map[string]uuid.UUID
	hashes    map[string][32]byte
	versions  []store.AgentVersion
	budgets   map[uuid.UUID]store.TaskBudgetStatus
	tools     []store.ToolDescriptor
	approvals map[uuid.UUID]store.ToolApproval
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		byID: map[uuid.UUID]store.Task{}, byKey: map[string]uuid.UUID{},
		hashes: map[string][32]byte{}, budgets: map[uuid.UUID]store.TaskBudgetStatus{},
		approvals: map[uuid.UUID]store.ToolApproval{},
	}
}

func (m *memoryStore) CreateTask(_ context.Context, in store.CreateTaskInput) (store.CreateTaskResult, error) {
	normalized, hash, err := in.ValidateAndHash()
	if err != nil {
		return store.CreateTaskResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := in.TenantID + "/" + in.Namespace + "/" + in.IdempotencyKey
	if id, ok := m.byKey[key]; ok {
		if m.hashes[key] != hash {
			return store.CreateTaskResult{}, store.ErrIdempotencyConflict
		}
		return store.CreateTaskResult{Task: m.byID[id], Existing: true}, nil
	}
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	task := store.Task{
		ID: in.ID, TenantID: in.TenantID, Namespace: in.Namespace, AgentVersionRef: in.AgentVersionRef,
		Goal: in.Goal, Spec: normalized, RequestHash: hash, IdempotencyKey: in.IdempotencyKey,
		Phase: domain.TaskQueued, ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
	}
	m.byID[task.ID] = task
	m.byKey[key] = task.ID
	m.hashes[key] = hash
	return store.CreateTaskResult{Task: task}, nil
}

func (m *memoryStore) GetTask(_ context.Context, tenantID string, id uuid.UUID) (store.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.byID[id]
	if !ok || task.TenantID != tenantID {
		return store.Task{}, store.ErrNotFound
	}
	return task, nil
}

func (m *memoryStore) RequestTaskCancellation(_ context.Context, tenantID string, id uuid.UUID, expectedVersion int64) (store.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.byID[id]
	if !ok || task.TenantID != tenantID {
		return store.Task{}, store.ErrNotFound
	}
	if task.ResourceVersion != expectedVersion {
		return store.Task{}, store.ErrVersionConflict
	}
	if err := domain.ValidateTaskTransition(task.Phase, domain.TaskCancelled); err != nil {
		return store.Task{}, errors.Join(store.ErrInvalidTransition, err)
	}
	now := task.UpdatedAt.Add(time.Second)
	task.Phase, task.ResourceVersion, task.UpdatedAt, task.CancelRequestedAt = domain.TaskCancelled, task.ResourceVersion+1, now, &now
	m.byID[id] = task
	return task, nil
}

func (m *memoryStore) CreateAgentVersion(_ context.Context, in store.CreateAgentVersionInput) (store.CreateAgentVersionResult, error) {
	canonical, digest, err := in.ValidateAndHash()
	if err != nil {
		return store.CreateAgentVersionResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.versions {
		if existing.TenantID == in.TenantID && existing.Name == in.Name && existing.Version == in.Version {
			if existing.SpecDigest != digest {
				return store.CreateAgentVersionResult{}, store.ErrAgentVersionConflict
			}
			return store.CreateAgentVersionResult{AgentVersion: existing, Existing: true}, nil
		}
	}
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	version := store.AgentVersion{
		ID: in.ID, TenantID: in.TenantID, Namespace: in.Namespace, Name: in.Name, Version: in.Version,
		Spec: canonical, SpecDigest: digest, ResourceVersion: 1, CreatedAt: now,
	}
	m.versions = append(m.versions, version)
	return store.CreateAgentVersionResult{AgentVersion: version}, nil
}

func (m *memoryStore) GetAgentVersion(_ context.Context, tenantID string, id uuid.UUID) (store.AgentVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, version := range m.versions {
		if version.ID == id && version.TenantID == tenantID {
			return version, nil
		}
	}
	return store.AgentVersion{}, store.ErrNotFound
}

func (m *memoryStore) GetAgentVersionByRef(_ context.Context, tenantID, ref string) (store.AgentVersion, error) {
	name, version, err := agentversion.ParseRef(ref)
	if err != nil {
		return store.AgentVersion{}, store.ErrAgentVersionRefInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, candidate := range m.versions {
		if candidate.TenantID == tenantID && candidate.Name == name && candidate.Version == version {
			return candidate, nil
		}
	}
	return store.AgentVersion{}, store.ErrNotFound
}

func (m *memoryStore) GetTaskBudget(_ context.Context, tenantID string, id uuid.UUID) (store.TaskBudgetStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	task, ok := m.byID[id]
	if !ok || task.TenantID != tenantID {
		return store.TaskBudgetStatus{}, store.ErrNotFound
	}
	status, ok := m.budgets[id]
	if !ok {
		return store.TaskBudgetStatus{}, store.ErrBudgetNotReserved
	}
	return status, nil
}

func (m *memoryStore) ListToolDescriptors(context.Context, string) ([]store.ToolDescriptor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.tools), nil
}

func (m *memoryStore) GetToolApproval(_ context.Context, tenantID string, id uuid.UUID) (store.ToolApproval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	approval, ok := m.approvals[id]
	if !ok || approval.TenantID != tenantID {
		return store.ToolApproval{}, store.ErrNotFound
	}
	return approval, nil
}

func (m *memoryStore) DecideToolApproval(_ context.Context, in store.DecideToolApprovalInput) (store.ToolApproval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	approval, ok := m.approvals[in.ApprovalID]
	if !ok || approval.TenantID != in.TenantID {
		return store.ToolApproval{}, store.ErrNotFound
	}
	if approval.ResourceVersion != in.ExpectedVersion {
		return store.ToolApproval{}, store.ErrVersionConflict
	}
	if approval.Status != store.ToolApprovalPending {
		return store.ToolApproval{}, store.ErrInvalidTransition
	}
	approval.Status = in.Decision
	approval.DecidedAt, approval.DecidedBy = &in.Now, in.DecidedBy
	approval.ResourceVersion++
	m.approvals[approval.ID] = approval
	return approval, nil
}

func (m *memoryStore) setBudget(id uuid.UUID, status store.TaskBudgetStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.budgets[id] = status
}

func TestListToolsReturnsRegisteredDescriptors(t *testing.T) {
	backend := newMemoryStore()
	backend.tools = []store.ToolDescriptor{
		{ID: uuid.New(), TenantID: "tenant-a", Name: "fs.read", Version: "1.0.0", SideEffectRisk: store.ToolRiskLow,
			Actions: []string{"read"}, ResourcePatterns: []string{"fs:/tmp"},
			ParamsSchema: json.RawMessage(`{"type":"object"}`)},
	}
	handler := auth.StaticMiddleware(
		auth.Principal{Subject: "user-1", TenantID: "tenant-a"},
		controlapi.NewHandler(backend, backend, backend),
	)
	request := httptest.NewRequest(http.MethodGet, "/v1/tools", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Tools) != 1 || body.Tools[0]["name"] != "fs.read" || body.Tools[0]["sideEffectRisk"] != "low" {
		t.Fatalf("unexpected tools response: %+v", body)
	}
}

func TestDecideApprovalRequiresBindingAndIfMatch(t *testing.T) {
	backend := newMemoryStore()
	approvalID := uuid.New()
	now := time.Now().UTC()
	backend.approvals[approvalID] = store.ToolApproval{
		ID: approvalID, TenantID: "tenant-a", CallID: uuid.New(), TaskID: uuid.New(),
		RunID: uuid.New(), AttemptID: uuid.New(), ToolName: "fs.write", ToolVersion: "1.0.0",
		Action: "write", Resource: "fs:/tmp", ArgsHash: [32]byte{1}, Status: store.ToolApprovalPending,
		RequestedAt: now, ExpiresAt: now.Add(time.Hour), ResourceVersion: 1,
	}
	handler := auth.StaticMiddleware(
		auth.Principal{Subject: "user-1", TenantID: "tenant-a"},
		controlapi.NewHandler(backend, backend, backend),
	)

	request := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+approvalID.String()+":decide",
		bytes.NewReader([]byte(`{"decision":"APPROVED","decidedBy":"human-1"}`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `W/"1"`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "APPROVED" || body["decidedBy"] != "human-1" || body["resourceVersion"] != float64(2) {
		t.Fatalf("unexpected approval response: %+v", body)
	}

	stale := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+approvalID.String()+":decide",
		bytes.NewReader([]byte(`{"decision":"REJECTED","decidedBy":"human-1"}`)))
	stale.Header.Set("Content-Type", "application/json")
	stale.Header.Set("If-Match", `W/"1"`)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, stale)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale decision status = %d: %s", response.Code, response.Body.String())
	}
	assertReason(t, response, "RESOURCE_VERSION_CONFLICT")

	invalid := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+approvalID.String()+":decide",
		bytes.NewReader([]byte(`{"decision":"MAYBE","decidedBy":"human-1"}`)))
	invalid.Header.Set("Content-Type", "application/json")
	invalid.Header.Set("If-Match", `W/"2"`)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, invalid)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid decision status = %d: %s", response.Code, response.Body.String())
	}
	assertReason(t, response, "INVALID_APPROVAL_DECISION")

	missing := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+uuid.New().String()+":decide",
		bytes.NewReader([]byte(`{"decision":"APPROVED","decidedBy":"human-1"}`)))
	missing.Header.Set("Content-Type", "application/json")
	missing.Header.Set("If-Match", `W/"1"`)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, missing)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing approval status = %d: %s", response.Code, response.Body.String())
	}
	assertReason(t, response, "APPROVAL_NOT_FOUND")
}

// TestTaskEventsStreamsLifecycle proves the SSE contract: the stream opens
// with the current snapshot, emits task.updated on every resource-version
// change, and closes with task.terminal once the task is terminal.
func TestTaskEventsStreamsLifecycle(t *testing.T) {
	backend := newMemoryStore()
	task, err := backend.CreateTask(context.Background(), store.CreateTaskInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent@1",
		Goal: "watch me", Spec: []byte(`{}`), IdempotencyKey: "watch-request-1",
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	handler := auth.StaticMiddleware(
		auth.Principal{Subject: "user-1", TenantID: "tenant-a"},
		controlapi.NewHandler(backend, backend, backend),
	)
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.Task.ID.String()+"/events?poll=100ms", nil)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	// Give the stream a moment to open, then advance the task to a terminal
	// phase; the stream must observe the change and close.
	time.Sleep(50 * time.Millisecond)
	if _, err := backend.RequestTaskCancellation(context.Background(), "tenant-a", task.Task.ID, 1); err != nil {
		t.Fatalf("advance task: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream did not close after the task became terminal")
	}

	body := response.Body.String()
	var initial, updated, terminal bool
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(frame) == "" || strings.HasPrefix(frame, ": keepalive") {
			continue
		}
		var event, id string
		var data string
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("decode event data %q: %v", data, err)
		}
		switch {
		case event == "task.updated" && id == "1" && payload["phase"] == "QUEUED":
			initial = true
		case event == "task.updated" && id == "2" && payload["phase"] == "CANCELLED":
			updated = true
		case event == "task.terminal" && id == "2" && payload["phase"] == "CANCELLED":
			terminal = true
		default:
			t.Fatalf("unexpected event frame: event=%q id=%q payload=%v", event, id, payload)
		}
	}
	if !initial || !updated || !terminal {
		t.Fatalf("stream incomplete: initial=%v updated=%v terminal=%v\n%s", initial, updated, terminal, body)
	}
}

// TestTaskEventsClosesImmediatelyForTerminalTask proves that connecting to an
// already-terminal task yields a single snapshot pair and an immediate close.
func TestTaskEventsClosesImmediatelyForTerminalTask(t *testing.T) {
	backend := newMemoryStore()
	task, err := backend.CreateTask(context.Background(), store.CreateTaskInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent@1",
		Goal: "done already", Spec: []byte(`{}`), IdempotencyKey: "watch-terminal-1",
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := backend.RequestTaskCancellation(context.Background(), "tenant-a", task.Task.ID, 1); err != nil {
		t.Fatalf("advance task: %v", err)
	}
	handler := auth.StaticMiddleware(
		auth.Principal{Subject: "user-1", TenantID: "tenant-a"},
		controlapi.NewHandler(backend, backend, backend),
	)
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.Task.ID.String()+"/events", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "event: task.terminal") {
		t.Fatalf("terminal task stream: %s", response.Body.String())
	}
}

// TestTaskEventsRejectsInvalidRequests proves the events route enforces the
// same tenant boundary and validates its poll parameter.
func TestTaskEventsRejectsInvalidRequests(t *testing.T) {
	backend := newMemoryStore()
	task, err := backend.CreateTask(context.Background(), store.CreateTaskInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent@1",
		Goal: "private", Spec: []byte(`{}`), IdempotencyKey: "watch-private-1",
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	handler := controlapi.NewHandler(backend, backend, backend)

	// Cross-tenant observation must fail closed.
	other := auth.StaticMiddleware(auth.Principal{Subject: "other", TenantID: "tenant-b"}, handler)
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.Task.ID.String()+"/events", nil)
	response := httptest.NewRecorder()
	other.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status = %d: %s", response.Code, response.Body.String())
	}

	// An invalid poll interval must be rejected up front, not streamed.
	owner := auth.StaticMiddleware(auth.Principal{Subject: "owner", TenantID: "tenant-a"}, handler)
	request = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+task.Task.ID.String()+"/events?poll=10s", nil)
	response = httptest.NewRecorder()
	owner.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid poll status = %d: %s", response.Code, response.Body.String())
	}
	assertReason(t, response, "INVALID_POLL_INTERVAL")

	// POST is not allowed on the stream resource.
	request = httptest.NewRequest(http.MethodPost, "/v1/tasks/"+task.Task.ID.String()+"/events", nil)
	response = httptest.NewRecorder()
	owner.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d: %s", response.Code, response.Body.String())
	}

	// Unknown tasks fail closed.
	request = httptest.NewRequest(http.MethodGet, "/v1/tasks/"+uuid.New().String()+"/events", nil)
	response = httptest.NewRecorder()
	owner.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown task status = %d: %s", response.Code, response.Body.String())
	}
}
