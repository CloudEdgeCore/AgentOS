// Package api implements the versioned public REST control contract.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/control/auth"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

const maxRequestBody = 1 << 20
const requestTimeout = 10 * time.Second

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type TaskStore interface {
	CreateTask(context.Context, store.CreateTaskInput) (store.CreateTaskResult, error)
	GetTask(context.Context, string, uuid.UUID) (store.Task, error)
	RequestTaskCancellation(context.Context, string, uuid.UUID, int64) (store.Task, error)
}

type Handler struct {
	store TaskStore
	newID func() uuid.UUID
}

func NewHandler(taskStore TaskStore) http.Handler {
	handler := &Handler{
		store: taskStore,
		newID: func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tasks", handler.createTask)
	mux.HandleFunc("GET /v1/tasks/{taskID}", handler.getTask)
	mux.HandleFunc("POST /v1/tasks/{taskAction}", handler.cancelTask)
	mux.HandleFunc("GET /healthz", handler.health)
	mux.HandleFunc("/v1/tasks", handler.methodNotAllowed)
	mux.HandleFunc("/v1/tasks/{taskID}", handler.methodNotAllowed)
	mux.HandleFunc("/healthz", handler.methodNotAllowed)
	mux.HandleFunc("/", handler.notFound)
	return handler.requestContext(mux)
}

type createTaskRequest struct {
	AgentVersionRef string          `json:"agentVersionRef"`
	Goal            string          `json:"goal"`
	Namespace       string          `json:"namespace"`
	Spec            json.RawMessage `json:"spec"`
}

type taskResponse struct {
	APIVersion        string          `json:"apiVersion"`
	Kind              string          `json:"kind"`
	ID                string          `json:"id"`
	TenantID          string          `json:"tenantId"`
	Namespace         string          `json:"namespace"`
	AgentVersionRef   string          `json:"agentVersionRef"`
	Goal              string          `json:"goal"`
	Spec              json.RawMessage `json:"spec"`
	Phase             string          `json:"phase"`
	ActiveRunID       *string         `json:"activeRunId"`
	ResultRef         *string         `json:"resultRef"`
	ReasonCode        *string         `json:"reasonCode"`
	CancelRequestedAt *time.Time      `json:"cancelRequestedAt"`
	ResourceVersion   int64           `json:"resourceVersion"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
	TraceID           string          `json:"traceId"`
}

type problem struct {
	Type       string `json:"type"`
	Title      string `json:"title"`
	Status     int    `json:"status"`
	Detail     string `json:"detail"`
	Instance   string `json:"instance"`
	ReasonCode string `json:"reasonCode"`
	TraceID    string `json:"traceId"`
}

func (h *Handler) createTask(writer http.ResponseWriter, request *http.Request) {
	traceID := traceIDFrom(request.Context())
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		h.writeProblem(writer, request, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", "authenticated principal is required", traceID)
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.writeProblem(writer, request, http.StatusUnsupportedMediaType, "CONTENT_TYPE_REQUIRED", "Content-Type must be application/json", traceID)
		return
	}
	idempotencyKey := request.Header.Get("Idempotency-Key")
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		h.writeProblem(writer, request, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Idempotency-Key must be 1..128 safe ASCII characters", traceID)
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	encoded, err := io.ReadAll(request.Body)
	if err != nil {
		h.writeDecodeProblem(writer, request, err, traceID)
		return
	}
	if err := rejectDuplicateJSONKeys(encoded); err != nil {
		h.writeProblem(writer, request, http.StatusBadRequest, "INVALID_JSON", err.Error(), traceID)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var body createTaskRequest
	if err := decoder.Decode(&body); err != nil {
		h.writeDecodeProblem(writer, request, err, traceID)
		return
	}
	if err := requireJSONEOF(decoder); err != nil {
		h.writeProblem(writer, request, http.StatusBadRequest, "INVALID_JSON", err.Error(), traceID)
		return
	}
	if err := validateCreateTask(body); err != nil {
		h.writeProblem(writer, request, http.StatusUnprocessableEntity, "INVALID_TASK", err.Error(), traceID)
		return
	}
	result, err := h.store.CreateTask(request.Context(), store.CreateTaskInput{
		ID: h.newID(), TenantID: principal.TenantID, Namespace: body.Namespace,
		AgentVersionRef: body.AgentVersionRef, Goal: body.Goal, Spec: body.Spec,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		h.writeStoreProblem(writer, request, err, traceID)
		return
	}
	status := http.StatusAccepted
	if result.Existing {
		status = http.StatusOK
		writer.Header().Set("Idempotent-Replayed", "true")
	}
	writer.Header().Set("Location", "/v1/tasks/"+result.Task.ID.String())
	h.writeTask(writer, status, result.Task, traceID)
}

func (h *Handler) getTask(writer http.ResponseWriter, request *http.Request) {
	traceID := traceIDFrom(request.Context())
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		h.writeProblem(writer, request, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", "authenticated principal is required", traceID)
		return
	}
	id, err := uuid.Parse(request.PathValue("taskID"))
	if err != nil {
		h.writeProblem(writer, request, http.StatusNotFound, "TASK_NOT_FOUND", "task was not found", traceID)
		return
	}
	task, err := h.store.GetTask(request.Context(), principal.TenantID, id)
	if err != nil {
		h.writeStoreProblem(writer, request, err, traceID)
		return
	}
	h.writeTask(writer, http.StatusOK, task, traceID)
}

func (h *Handler) cancelTask(writer http.ResponseWriter, request *http.Request) {
	traceID := traceIDFrom(request.Context())
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		h.writeProblem(writer, request, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", "authenticated principal is required", traceID)
		return
	}
	taskID, ok := strings.CutSuffix(request.PathValue("taskAction"), ":cancel")
	if !ok {
		h.notFound(writer, request)
		return
	}
	id, err := uuid.Parse(taskID)
	if err != nil {
		h.writeProblem(writer, request, http.StatusNotFound, "TASK_NOT_FOUND", "task was not found", traceID)
		return
	}
	expectedVersion, err := parseEntityVersion(request.Header.Get("If-Match"))
	if err != nil {
		h.writeProblem(writer, request, http.StatusPreconditionRequired, "IF_MATCH_REQUIRED", "If-Match must contain the current weak resource-version ETag", traceID)
		return
	}
	task, err := h.store.RequestTaskCancellation(request.Context(), principal.TenantID, id, expectedVersion)
	if err != nil {
		h.writeStoreProblem(writer, request, err, traceID)
		return
	}
	h.writeTask(writer, http.StatusAccepted, task, traceID)
}

func (h *Handler) health(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, `{"status":"ok"}`)
}

func (h *Handler) methodNotAllowed(writer http.ResponseWriter, request *http.Request) {
	h.writeProblem(writer, request, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method is not allowed for this resource", traceIDFrom(request.Context()))
}

func (h *Handler) notFound(writer http.ResponseWriter, request *http.Request) {
	h.writeProblem(writer, request, http.StatusNotFound, "ROUTE_NOT_FOUND", "route was not found", traceIDFrom(request.Context()))
}

func (h *Handler) requestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), requestTimeout)
		defer cancel()
		traceID := request.Header.Get("X-Request-ID")
		if _, err := uuid.Parse(traceID); err != nil {
			traceID = h.newID().String()
		}
		writer.Header().Set("X-Request-ID", traceID)
		next.ServeHTTP(writer, request.WithContext(context.WithValue(ctx, traceKey{}, traceID)))
	})
}

func (h *Handler) writeTask(writer http.ResponseWriter, status int, task store.Task, traceID string) {
	response := taskResponse{
		APIVersion: "agentos.dev/v1alpha1", Kind: "Task", ID: task.ID.String(),
		TenantID: task.TenantID, Namespace: task.Namespace, AgentVersionRef: task.AgentVersionRef,
		Goal: task.Goal, Spec: task.Spec, Phase: string(task.Phase), ResourceVersion: task.ResourceVersion,
		CreatedAt: task.CreatedAt.UTC(), UpdatedAt: task.UpdatedAt.UTC(), TraceID: traceID,
	}
	if task.ActiveRunID != nil {
		value := task.ActiveRunID.String()
		response.ActiveRunID = &value
	}
	if task.ResultRef != "" {
		response.ResultRef = &task.ResultRef
	}
	if task.AdmissionReasonCode != "" {
		response.ReasonCode = &task.AdmissionReasonCode
	}
	if task.CancelRequestedAt != nil {
		value := task.CancelRequestedAt.UTC()
		response.CancelRequestedAt = &value
	}
	writer.Header().Set("ETag", fmt.Sprintf(`W/"%d"`, task.ResourceVersion))
	writeJSON(writer, status, response)
}

func (h *Handler) writeStoreProblem(writer http.ResponseWriter, request *http.Request, err error, traceID string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.writeProblem(writer, request, http.StatusNotFound, "TASK_NOT_FOUND", "task was not found", traceID)
	case errors.Is(err, store.ErrIdempotencyConflict):
		h.writeProblem(writer, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "idempotency key was already used for a different request", traceID)
	case errors.Is(err, store.ErrVersionConflict):
		h.writeProblem(writer, request, http.StatusConflict, "RESOURCE_VERSION_CONFLICT", "resource changed concurrently", traceID)
	case errors.Is(err, store.ErrInvalidTransition):
		h.writeProblem(writer, request, http.StatusConflict, "TASK_STATE_CONFLICT", "task lifecycle does not allow this operation", traceID)
	default:
		h.writeProblem(writer, request, http.StatusInternalServerError, "INTERNAL_ERROR", "request could not be completed", traceID)
	}
}

func (h *Handler) writeDecodeProblem(writer http.ResponseWriter, request *http.Request, err error, traceID string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		h.writeProblem(writer, request, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body exceeds 1 MiB", traceID)
		return
	}
	h.writeProblem(writer, request, http.StatusBadRequest, "INVALID_JSON", "request body is not valid for this API version", traceID)
}

func (h *Handler) writeProblem(writer http.ResponseWriter, request *http.Request, status int, reasonCode, detail, traceID string) {
	writer.Header().Set("Content-Type", "application/problem+json")
	writeJSON(writer, status, problem{
		Type:  "https://agentos.dev/problems/" + strings.ToLower(strings.ReplaceAll(reasonCode, "_", "-")),
		Title: http.StatusText(status), Status: status, Detail: detail,
		Instance: request.URL.Path, ReasonCode: reasonCode, TraceID: traceID,
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	if writer.Header().Get("Content-Type") == "" {
		writer.Header().Set("Content-Type", "application/json")
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func validateCreateTask(body createTaskRequest) error {
	if strings.TrimSpace(body.AgentVersionRef) == "" || len(body.AgentVersionRef) > 512 {
		return fmt.Errorf("agentVersionRef is required and must not exceed 512 bytes")
	}
	if strings.TrimSpace(body.Goal) == "" || len(body.Goal) > 16*1024 {
		return fmt.Errorf("goal is required and must not exceed 16 KiB")
	}
	if strings.TrimSpace(body.Namespace) == "" || len(body.Namespace) > 255 {
		return fmt.Errorf("namespace is required and must not exceed 255 bytes")
	}
	if len(body.Spec) == 0 || string(body.Spec) == "null" {
		return fmt.Errorf("spec is required")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body.Spec, &object); err != nil || object == nil {
		return fmt.Errorf("spec must be a JSON object")
	}
	return nil
}

func parseEntityVersion(value string) (int64, error) {
	if len(value) < 5 || !strings.HasPrefix(value, `W/"`) || !strings.HasSuffix(value, `"`) {
		return 0, fmt.Errorf("weak ETag is required")
	}
	version, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(value, `W/"`), `"`), 10, 64)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("weak ETag is invalid")
	}
	if value != fmt.Sprintf(`W/"%d"`, version) {
		return 0, fmt.Errorf("weak ETag is not canonical")
	}
	return version, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return fmt.Errorf("request body must contain exactly one JSON document")
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("request body contains invalid or ambiguous JSON")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("array is not closed")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	return nil
}

type traceKey struct{}

func traceIDFrom(ctx context.Context) string {
	value, _ := ctx.Value(traceKey{}).(string)
	return value
}
