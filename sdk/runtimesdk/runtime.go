// Package runtimesdk provides the developer kit for building third-party AgentOS Runtimes
// conforming to agentos.runtime.interface/v1.
package runtimesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/CloudEdgeCore/AgentOS/sdk/agent"
)

// LifecycleHandler defines the 7 explicit lifecycle methods a third-party runtime implements:
// health / start / event / result / checkpoint / restore / stop.
type LifecycleHandler interface {
	Health(ctx context.Context) (agent.HealthResponse, error)
	Start(ctx context.Context, req agent.StartRequest) (agent.StartResponse, error)
	Event(ctx context.Context, executionID string, after int64) (agent.EventList, error)
	Result(ctx context.Context, executionID string) (agent.Result, error)
	Checkpoint(ctx context.Context, executionID string) (agent.CheckpointResponse, error)
	Restore(ctx context.Context, req agent.RestoreRequest) (agent.RestoreResponse, error)
	Stop(ctx context.Context, executionID string) (agent.StopResponse, error)
}

// HandlerToHTTP creates an http.Handler serving the agentos.runtime.interface/v1 specification
// from a LifecycleHandler implementation.
func HandlerToHTTP(h LifecycleHandler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		resp, err := h.Health(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req agent.StartRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Capability verification: default-deny capabilities
		if req.Capabilities.Secrets == nil {
			http.Error(w, "implicit capabilities denied: secrets array cannot be nil", http.StatusUnprocessableEntity)
			return
		}
		resp, err := h.Start(r.Context(), req)
		if err != nil {
			if err == agent.ErrExecutionConflict {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		executionID := r.URL.Query().Get("executionId")
		afterStr := r.URL.Query().Get("after")
		var after int64
		if afterStr != "" {
			fmt.Sscanf(afterStr, "%d", &after)
		}
		resp, err := h.Event(r.Context(), executionID, after)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		executionID := r.URL.Query().Get("executionId")
		resp, err := h.Result(r.Context(), executionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/checkpoint", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ExecutionID string `json:"executionId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := h.Checkpoint(r.Context(), req.ExecutionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/restore", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req agent.RestoreRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := h.Restore(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ExecutionID string `json:"executionId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := h.Stop(r.Context(), req.ExecutionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	return mux
}

// Serve starts an HTTP server for the given LifecycleHandler on the specified address.
func Serve(h LifecycleHandler, addr string) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           HandlerToHTTP(h),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return server.ListenAndServe()
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
