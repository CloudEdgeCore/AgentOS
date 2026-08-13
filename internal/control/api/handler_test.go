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
	"github.com/bian-cloud-skill/agentos/internal/kernel/domain"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

func TestCreateTaskRequiresIdentityAndIdempotency(t *testing.T) {
	handler := controlapi.NewHandler(newMemoryStore())
	body := []byte(`{"agentVersionRef":"agent:v1","goal":"test","namespace":"default","spec":{}}`)

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
		controlapi.NewHandler(backend),
	)
	body := []byte(`{"agentVersionRef":"agent:v1","goal":"test","namespace":"default","spec":{"runtimeClass":"oci"}}`)

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

	conflict := performCreate(handler, []byte(`{"agentVersionRef":"agent:v1","goal":"different","namespace":"default","spec":{}}`), "request-1")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d: %s", conflict.Code, conflict.Body.String())
	}
	assertReason(t, conflict, "IDEMPOTENCY_CONFLICT")

	unknown := performCreate(handler, []byte(`{"agentVersionRef":"agent:v1","goal":"test","namespace":"default","spec":{},"status":"SUCCEEDED"}`), "request-2")
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d: %s", unknown.Code, unknown.Body.String())
	}
	assertReason(t, unknown, "INVALID_JSON")

	duplicate := performCreate(handler, []byte(`{"agentVersionRef":"agent:v1","goal":"first","goal":"second","namespace":"default","spec":{"budget":{"tokens":1,"tokens":2}}}`), "request-3")
	if duplicate.Code != http.StatusBadRequest {
		t.Fatalf("duplicate key status = %d: %s", duplicate.Code, duplicate.Body.String())
	}
	assertReason(t, duplicate, "INVALID_JSON")
}

func TestGetTaskDoesNotCrossTenantBoundary(t *testing.T) {
	backend := newMemoryStore()
	task, err := backend.CreateTask(context.Background(), store.CreateTaskInput{
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent:v1",
		Goal: "private", Spec: []byte(`{}`), IdempotencyKey: "request-1",
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	handler := controlapi.NewHandler(backend)

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
		ID: uuid.New(), TenantID: "tenant-a", Namespace: "default", AgentVersionRef: "agent:v1",
		Goal: "cancel me", Spec: []byte(`{}`), IdempotencyKey: "cancel-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := auth.StaticMiddleware(auth.Principal{Subject: "owner", TenantID: "tenant-a"}, controlapi.NewHandler(backend))
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

func TestRoutingFailuresRemainStructured(t *testing.T) {
	handler := controlapi.NewHandler(newMemoryStore())
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
	mu     sync.Mutex
	byID   map[uuid.UUID]store.Task
	byKey  map[string]uuid.UUID
	hashes map[string][32]byte
}

func newMemoryStore() *memoryStore {
	return &memoryStore{byID: map[uuid.UUID]store.Task{}, byKey: map[string]uuid.UUID{}, hashes: map[string][32]byte{}}
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
