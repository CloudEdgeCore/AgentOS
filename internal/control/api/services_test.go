package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	controlapi "github.com/CloudEdgeCore/AgentOS/internal/control/api"
	"github.com/CloudEdgeCore/AgentOS/internal/control/auth"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/supervisor"
)

func TestServiceEndpointsLifecycle(t *testing.T) {
	backend := newMemoryStore()
	serviceStore := supervisor.NewMemoryStore()
	sup := supervisor.NewSupervisor(serviceStore)

	handler := controlapi.NewHandler(backend, backend, backend, backend,
		controlapi.WithSupervisor(sup, serviceStore),
	)
	authed := auth.StaticMiddleware(auth.Principal{Subject: "user-1", TenantID: "tenant-a"}, handler)

	// 1. Create a service
	createBody := []byte(`{
		"namespace": "default",
		"name": "summarizer-svc",
		"agentId": "summarizer",
		"spec": {
			"replicas": 2,
			"restartPolicy": "Always"
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/services", bytes.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	authed.ServeHTTP(resp, req)

	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", resp.Code, resp.Body.String())
	}

	var created map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created service: %v", err)
	}
	serviceID, ok := created["id"].(string)
	if !ok || serviceID == "" {
		t.Fatalf("missing service ID in response")
	}

	// 2. Get service
	req = httptest.NewRequest(http.MethodGet, "/v1/services/"+serviceID, nil)
	resp = httptest.NewRecorder()
	authed.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on get, got %d: %s", resp.Code, resp.Body.String())
	}

	// 3. List services
	req = httptest.NewRequest(http.MethodGet, "/v1/services?namespace=default", nil)
	resp = httptest.NewRecorder()
	authed.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on list, got %d: %s", resp.Code, resp.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list services: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 service, got %d", len(list))
	}

	// 4. List instances (should have 2 spawned by supervisor)
	req = httptest.NewRequest(http.MethodGet, "/v1/services/"+serviceID+"/instances", nil)
	resp = httptest.NewRecorder()
	authed.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on list instances, got %d: %s", resp.Code, resp.Body.String())
	}
	var instances []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &instances); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("expected 2 instances, got %d", len(instances))
	}
	instanceID := instances[0]["id"].(string)

	// 5. Heartbeat instance
	req = httptest.NewRequest(http.MethodPost, "/v1/services/"+serviceID+"/instances/"+instanceID+"/heartbeat", nil)
	resp = httptest.NewRecorder()
	authed.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204 NoContent on heartbeat, got %d: %s", resp.Code, resp.Body.String())
	}

	// 6. Scale service to 1
	scaleBody := []byte(`{"replicas": 1}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/services/"+serviceID+"/scale", bytes.NewReader(scaleBody))
	req.Header.Set("Content-Type", "application/json")
	resp = httptest.NewRecorder()
	authed.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on scale, got %d: %s", resp.Code, resp.Body.String())
	}

	// 7. Restart service
	req = httptest.NewRequest(http.MethodPost, "/v1/services/"+serviceID+"/restart", nil)
	resp = httptest.NewRecorder()
	authed.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on restart, got %d: %s", resp.Code, resp.Body.String())
	}

	// 8. Delete service
	req = httptest.NewRequest(http.MethodDelete, "/v1/services/"+serviceID, nil)
	resp = httptest.NewRecorder()
	authed.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204 NoContent on delete, got %d: %s", resp.Code, resp.Body.String())
	}

	// 9. Get deleted service -> 404
	req = httptest.NewRequest(http.MethodGet, "/v1/services/"+serviceID, nil)
	resp = httptest.NewRecorder()
	authed.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on get deleted service, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestServiceEndpointsDisabledWhenNoSupervisor(t *testing.T) {
	backend := newMemoryStore()
	handler := controlapi.NewHandler(backend, backend, backend, backend)
	authed := auth.StaticMiddleware(auth.Principal{Subject: "user-1", TenantID: "tenant-a"}, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
	resp := httptest.NewRecorder()
	authed.ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when supervisor is disabled, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestServiceEndpointsRequireAuthentication(t *testing.T) {
	backend := newMemoryStore()
	serviceStore := supervisor.NewMemoryStore()
	sup := supervisor.NewSupervisor(serviceStore)
	handler := controlapi.NewHandler(backend, backend, backend, backend,
		controlapi.WithSupervisor(sup, serviceStore),
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d: %s", resp.Code, resp.Body.String())
	}
}
