package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	handler := controlapi.NewHandler(newMemoryStore(), newMemoryStore())
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
		controlapi.NewHandler(backend, backend),
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
	handler := controlapi.NewHandler(backend, backend)

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
	handler := auth.StaticMiddleware(auth.Principal{Subject: "owner", TenantID: "tenant-a"}, controlapi.NewHandler(backend, backend))
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
		controlapi.NewHandler(backend, backend),
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
	handler := controlapi.NewHandler(backend, backend)

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
	handler := auth.StaticMiddleware(auth.Principal{Subject: "owner", TenantID: "tenant-a"}, controlapi.NewHandler(backend, backend))
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
	handler := controlapi.NewHandler(newMemoryStore(), newMemoryStore())
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
	mu       sync.Mutex
	byID     map[uuid.UUID]store.Task
	byKey    map[string]uuid.UUID
	hashes   map[string][32]byte
	versions []store.AgentVersion
	budgets  map[uuid.UUID]store.TaskBudgetStatus
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		byID: map[uuid.UUID]store.Task{}, byKey: map[string]uuid.UUID{},
		hashes: map[string][32]byte{}, budgets: map[uuid.UUID]store.TaskBudgetStatus{},
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

func (m *memoryStore) setBudget(id uuid.UUID, status store.TaskBudgetStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.budgets[id] = status
}
