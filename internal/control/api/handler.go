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
	"github.com/bian-cloud-skill/agentos/internal/kernel/agentversion"
	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

const maxRequestBody = 1 << 20
const requestTimeout = 10 * time.Second

// Task event stream configuration. Polling keeps PostgreSQL as the source of
// truth (ADR-002): the stream observes resource-version changes instead of
// duplicating state in the API process.
const (
	defaultEventPoll     = 500 * time.Millisecond
	minEventPoll         = 100 * time.Millisecond
	maxEventPoll         = 5 * time.Second
	sseKeepAliveInterval = 15 * time.Second
	eventStreamMediaType = "text/event-stream"
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type TaskStore interface {
	CreateTask(context.Context, store.CreateTaskInput) (store.CreateTaskResult, error)
	GetTask(context.Context, string, uuid.UUID) (store.Task, error)
	RequestTaskCancellation(context.Context, string, uuid.UUID, int64) (store.Task, error)
	GetTaskBudget(context.Context, string, uuid.UUID) (store.TaskBudgetStatus, error)
}

type AgentVersionStore interface {
	CreateAgentVersion(context.Context, store.CreateAgentVersionInput) (store.CreateAgentVersionResult, error)
	GetAgentVersion(context.Context, string, uuid.UUID) (store.AgentVersion, error)
}

// ApprovalStore is the human decision surface for high-risk tool calls:
// capability discovery and bound approval decisions.
type ApprovalStore interface {
	ListToolDescriptors(context.Context, string) ([]store.ToolDescriptor, error)
	GetToolApproval(context.Context, string, uuid.UUID) (store.ToolApproval, error)
	DecideToolApproval(context.Context, store.DecideToolApprovalInput) (store.ToolApproval, error)
}

type Handler struct {
	tasks        TaskStore
	agentVersion AgentVersionStore
	approvals    ApprovalStore
	newID        func() uuid.UUID
}

func NewHandler(taskStore TaskStore, agentVersions AgentVersionStore, approvals ApprovalStore) http.Handler {
	handler := &Handler{
		tasks:        taskStore,
		agentVersion: agentVersions,
		approvals:    approvals,
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
	mux.HandleFunc("GET /v1/tasks/{taskID}/events", handler.taskEvents)
	mux.HandleFunc("POST /v1/tasks/{taskAction}", handler.cancelTask)
	mux.HandleFunc("POST /v1/agent-versions", handler.createAgentVersion)
	mux.HandleFunc("GET /v1/agent-versions/{agentVersionID}", handler.getAgentVersion)
	mux.HandleFunc("GET /v1/tools", handler.listTools)
	mux.HandleFunc("POST /v1/approvals/{approvalAction}", handler.decideApproval)
	mux.HandleFunc("GET /healthz", handler.health)
	mux.HandleFunc("/v1/tasks", handler.methodNotAllowed)
	mux.HandleFunc("/v1/tasks/{taskID}", handler.methodNotAllowed)
	mux.HandleFunc("/v1/tasks/{taskID}/events", handler.methodNotAllowed)
	mux.HandleFunc("/v1/agent-versions", handler.methodNotAllowed)
	mux.HandleFunc("/v1/agent-versions/{agentVersionID}", handler.methodNotAllowed)
	mux.HandleFunc("/v1/tools", handler.methodNotAllowed)
	mux.HandleFunc("/v1/approvals/{approvalAction}", handler.methodNotAllowed)
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

type usageSummary struct {
	Tokens      int64   `json:"tokens"`
	CostUSD     float64 `json:"costUsd"`
	ToolCalls   int64   `json:"toolCalls"`
	WallSeconds int64   `json:"wallSeconds"`
}

type taskResponse struct {
	APIVersion        string          `json:"apiVersion"`
	Kind              string          `json:"kind"`
	ID                string          `json:"id"`
	TenantID          string          `json:"tenantId"`
	Namespace         string          `json:"namespace"`
	AgentVersionRef   string          `json:"agentVersionRef"`
	AgentVersionID    *string         `json:"agentVersionId"`
	Goal              string          `json:"goal"`
	Spec              json.RawMessage `json:"spec"`
	Phase             string          `json:"phase"`
	ActiveRunID       *string         `json:"activeRunId"`
	ResultRef         *string         `json:"resultRef"`
	ReasonCode        *string         `json:"reasonCode"`
	CancelRequestedAt *time.Time      `json:"cancelRequestedAt"`
	Usage             usageSummary    `json:"usage"`
	BudgetExhausted   bool            `json:"budgetExhausted"`
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
	result, err := h.tasks.CreateTask(request.Context(), store.CreateTaskInput{
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
	h.writeTask(writer, status, result.Task, usageSummary{}, false, traceID)
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
	task, err := h.tasks.GetTask(request.Context(), principal.TenantID, id)
	if err != nil {
		h.writeStoreProblem(writer, request, err, traceID)
		return
	}
	usage, budgetExhausted := h.readUsage(request.Context(), principal.TenantID, id)
	h.writeTask(writer, http.StatusOK, task, usage, budgetExhausted, traceID)
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
	task, err := h.tasks.RequestTaskCancellation(request.Context(), principal.TenantID, id, expectedVersion)
	if err != nil {
		h.writeStoreProblem(writer, request, err, traceID)
		return
	}
	usage, budgetExhausted := h.readUsage(request.Context(), principal.TenantID, id)
	h.writeTask(writer, http.StatusAccepted, task, usage, budgetExhausted, traceID)
}

// taskEvents streams task lifecycle events over Server-Sent Events
// (GET /v1/tasks/{taskID}/events). The stream opens with the current task
// state and then emits task.updated on every resource-version change, with a
// final task.terminal event closing the stream once the task reaches a
// terminal phase. Comment heartbeats keep proxies from timing the connection
// out. Events are best-effort: the cursor is the resource version, so clients
// must reconcile with GET /v1/tasks/{taskID} after a reconnect.
func (h *Handler) taskEvents(writer http.ResponseWriter, request *http.Request) {
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
	task, err := h.tasks.GetTask(request.Context(), principal.TenantID, id)
	if err != nil {
		h.writeStoreProblem(writer, request, err, traceID)
		return
	}
	pollInterval := defaultEventPoll
	if raw := request.URL.Query().Get("poll"); raw != "" {
		parsed, parseErr := time.ParseDuration(raw)
		if parseErr != nil || parsed < minEventPoll || parsed > maxEventPoll {
			h.writeProblem(writer, request, http.StatusBadRequest, "INVALID_POLL_INTERVAL",
				"poll must be a duration between 100ms and 5s", traceID)
			return
		}
		pollInterval = parsed
	}
	controller := http.NewResponseController(writer)
	writer.Header().Set("Content-Type", eventStreamMediaType)
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Accel-Buffering", "no")
	// The stream outlives the normal request lifecycle: lift the write
	// deadline so idle connections are not killed mid-stream. Backends that
	// cannot set deadlines (e.g. test recorders) are fine — the stream simply
	// inherits the server's default behavior.
	if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		h.writeProblem(writer, request, http.StatusInternalServerError, "EVENTS_UNSUPPORTED",
			"streaming is not supported by this server", traceID)
		return
	}
	writer.WriteHeader(http.StatusOK)
	usage, budgetExhausted := h.readUsage(request.Context(), principal.TenantID, id)
	if !writeTaskEvent(writer, controller, "task.updated", task, usage, budgetExhausted, traceID) {
		return
	}
	if task.Phase.Terminal() {
		// Already terminal at connect: the stream is a single snapshot.
		_ = writeTaskEvent(writer, controller, "task.terminal", task, usage, budgetExhausted, traceID)
		return
	}
	lastVersion := task.ResourceVersion
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	keepAlive := time.NewTicker(sseKeepAliveInterval)
	defer keepAlive.Stop()
	for {
		select {
		case <-request.Context().Done():
			// The client disconnected: end the stream.
			return
		case <-keepAlive.C:
			if !writeSSEKeepAlive(writer, controller) {
				return
			}
		case <-poll.C:
			current, err := h.tasks.GetTask(request.Context(), principal.TenantID, id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					// The task vanished: close the stream; the client
					// reconciles with the resource endpoint.
					return
				}
				// Transient store failure: keep the stream alive and retry on
				// the next tick instead of dropping every subscriber.
				continue
			}
			if current.ResourceVersion == lastVersion {
				continue
			}
			lastVersion = current.ResourceVersion
			usage, budgetExhausted := h.readUsage(request.Context(), principal.TenantID, id)
			if !writeTaskEvent(writer, controller, "task.updated", current, usage, budgetExhausted, traceID) {
				return
			}
			if current.Phase.Terminal() {
				_ = writeTaskEvent(writer, controller, "task.terminal", current, usage, budgetExhausted, traceID)
				return
			}
		}
	}
}

// writeTaskEvent emits one SSE event carrying a Task snapshot. It reports
// whether the client is still connected.
func writeTaskEvent(writer http.ResponseWriter, controller *http.ResponseController, event string, task store.Task, usage usageSummary, budgetExhausted bool, traceID string) bool {
	payload, err := json.Marshal(taskResponseFrom(task, usage, budgetExhausted, traceID))
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(writer, "event: %s\nid: %d\ndata: %s\n\n", event, task.ResourceVersion, payload); err != nil {
		return false
	}
	return controller.Flush() == nil
}

// writeSSEKeepAlive emits a comment frame that proxies treat as activity.
func writeSSEKeepAlive(writer http.ResponseWriter, controller *http.ResponseController) bool {
	if _, err := io.WriteString(writer, ": keepalive\n\n"); err != nil {
		return false
	}
	return controller.Flush() == nil
}

// readUsage reads the cumulative consumed budget dimensions of a task.
func (h *Handler) readUsage(ctx context.Context, tenantID string, id uuid.UUID) (usageSummary, bool) {
	status, err := h.tasks.GetTaskBudget(ctx, tenantID, id)
	if err != nil {
		return usageSummary{}, false
	}
	return usageSummary{
		Tokens: status.Consumed.Tokens, CostUSD: status.Consumed.CostUSD,
		ToolCalls: status.Consumed.ToolCalls, WallSeconds: status.Consumed.WallSeconds,
	}, status.Exhausted
}

type createAgentVersionRequest struct {
	Name      string          `json:"name"`
	Version   string          `json:"version"`
	Namespace string          `json:"namespace"`
	Spec      json.RawMessage `json:"spec"`
}

type agentVersionResponse struct {
	APIVersion      string          `json:"apiVersion"`
	Kind            string          `json:"kind"`
	ID              string          `json:"id"`
	TenantID        string          `json:"tenantId"`
	Namespace       string          `json:"namespace"`
	Name            string          `json:"name"`
	Version         string          `json:"version"`
	Ref             string          `json:"ref"`
	Spec            json.RawMessage `json:"spec"`
	SpecDigest      string          `json:"specDigest"`
	ResourceVersion int64           `json:"resourceVersion"`
	CreatedAt       time.Time       `json:"createdAt"`
	TraceID         string          `json:"traceId"`
}

func (h *Handler) createAgentVersion(writer http.ResponseWriter, request *http.Request) {
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
	var body createAgentVersionRequest
	if err := decoder.Decode(&body); err != nil {
		h.writeDecodeProblem(writer, request, err, traceID)
		return
	}
	if err := requireJSONEOF(decoder); err != nil {
		h.writeProblem(writer, request, http.StatusBadRequest, "INVALID_JSON", err.Error(), traceID)
		return
	}
	if err := validateCreateAgentVersion(body); err != nil {
		h.writeProblem(writer, request, http.StatusUnprocessableEntity, "INVALID_AGENT_VERSION", err.Error(), traceID)
		return
	}
	result, err := h.agentVersion.CreateAgentVersion(request.Context(), store.CreateAgentVersionInput{
		ID: h.newID(), TenantID: principal.TenantID, Namespace: body.Namespace,
		Name: body.Name, Version: body.Version, Spec: body.Spec,
	})
	if err != nil {
		h.writeStoreProblem(writer, request, err, traceID)
		return
	}
	status := http.StatusCreated
	if result.Existing {
		status = http.StatusOK
		writer.Header().Set("Idempotent-Replayed", "true")
	}
	writer.Header().Set("Location", "/v1/agent-versions/"+result.AgentVersion.ID.String())
	h.writeAgentVersion(writer, status, result.AgentVersion, traceID)
}

func (h *Handler) getAgentVersion(writer http.ResponseWriter, request *http.Request) {
	traceID := traceIDFrom(request.Context())
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		h.writeProblem(writer, request, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", "authenticated principal is required", traceID)
		return
	}
	id, err := uuid.Parse(request.PathValue("agentVersionID"))
	if err != nil {
		h.writeProblem(writer, request, http.StatusNotFound, "AGENT_VERSION_NOT_FOUND", "agent version was not found", traceID)
		return
	}
	version, err := h.agentVersion.GetAgentVersion(request.Context(), principal.TenantID, id)
	if err != nil {
		h.writeStoreProblem(writer, request, err, traceID)
		return
	}
	h.writeAgentVersion(writer, http.StatusOK, version, traceID)
}

// listTools exposes the tenant's registered tool descriptors for capability
// discovery.
func (h *Handler) listTools(writer http.ResponseWriter, request *http.Request) {
	traceID := traceIDFrom(request.Context())
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		h.writeProblem(writer, request, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", "authenticated principal is required", traceID)
		return
	}
	descriptors, err := h.approvals.ListToolDescriptors(request.Context(), principal.TenantID)
	if err != nil {
		h.writeProblem(writer, request, http.StatusInternalServerError, "INTERNAL_ERROR", "request could not be completed", traceID)
		return
	}
	tools := make([]toolDescriptorResponse, 0, len(descriptors))
	for _, descriptor := range descriptors {
		tools = append(tools, toolDescriptorResponse{
			Name: descriptor.Name, Version: descriptor.Version,
			SideEffectRisk: string(descriptor.SideEffectRisk),
			Actions:        descriptor.Actions, ResourcePatterns: descriptor.ResourcePatterns,
			ParamsSchema: descriptor.ParamsSchema, SpecDigest: fmt.Sprintf("%x", descriptor.SpecHash),
			CreatedAt: descriptor.CreatedAt,
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"apiVersion": "agentos.dev/v1alpha1", "kind": "ToolList", "tools": tools, "traceId": traceID,
	})
}

type decideApprovalRequest struct {
	Decision  string `json:"decision"`
	DecidedBy string `json:"decidedBy"`
}

type toolApprovalResponse struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"tenantId"`
	CallID          string     `json:"callId"`
	TaskID          string     `json:"taskId"`
	RunID           string     `json:"runId"`
	AttemptID       string     `json:"attemptId"`
	ToolName        string     `json:"toolName"`
	ToolVersion     string     `json:"toolVersion"`
	Action          string     `json:"action"`
	Resource        string     `json:"resource"`
	ArgsHash        string     `json:"argsHash"`
	Status          string     `json:"status"`
	RequestedAt     time.Time  `json:"requestedAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	DecidedAt       *time.Time `json:"decidedAt,omitempty"`
	DecidedBy       string     `json:"decidedBy,omitempty"`
	ResourceVersion int64      `json:"resourceVersion"`
	TraceID         string     `json:"traceId"`
}

type toolDescriptorResponse struct {
	Name             string          `json:"name"`
	Version          string          `json:"version"`
	SideEffectRisk   string          `json:"sideEffectRisk"`
	Actions          []string        `json:"actions"`
	ResourcePatterns []string        `json:"resourcePatterns"`
	ParamsSchema     json.RawMessage `json:"paramsSchema"`
	SpecDigest       string          `json:"specDigest"`
	CreatedAt        time.Time       `json:"createdAt"`
}

// decideApproval is the human decision endpoint for high-risk tool calls
// (POST /v1/approvals/{approvalId}:decide). The decision is bound to the
// canonical call summary and expires with the approval.
func (h *Handler) decideApproval(writer http.ResponseWriter, request *http.Request) {
	traceID := traceIDFrom(request.Context())
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		h.writeProblem(writer, request, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", "authenticated principal is required", traceID)
		return
	}
	approvalRef, ok := strings.CutSuffix(request.PathValue("approvalAction"), ":decide")
	if !ok {
		h.notFound(writer, request)
		return
	}
	id, err := uuid.Parse(approvalRef)
	if err != nil {
		h.writeProblem(writer, request, http.StatusNotFound, "APPROVAL_NOT_FOUND", "approval was not found", traceID)
		return
	}
	expectedVersion, err := parseEntityVersion(request.Header.Get("If-Match"))
	if err != nil {
		h.writeProblem(writer, request, http.StatusPreconditionRequired, "IF_MATCH_REQUIRED", "If-Match must contain the current weak resource-version ETag", traceID)
		return
	}
	var body decideApprovalRequest
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
	if err := decoder.Decode(&body); err != nil {
		h.writeDecodeProblem(writer, request, err, traceID)
		return
	}
	if err := requireJSONEOF(decoder); err != nil {
		h.writeProblem(writer, request, http.StatusBadRequest, "INVALID_JSON", err.Error(), traceID)
		return
	}
	decision := store.ToolApprovalStatus(body.Decision)
	if decision != store.ToolApprovalApproved && decision != store.ToolApprovalRejected {
		h.writeProblem(writer, request, http.StatusUnprocessableEntity, "INVALID_APPROVAL_DECISION",
			"decision must be APPROVED or REJECTED", traceID)
		return
	}
	if strings.TrimSpace(body.DecidedBy) == "" {
		h.writeProblem(writer, request, http.StatusUnprocessableEntity, "DECIDED_BY_REQUIRED",
			"decidedBy must identify the human decision maker", traceID)
		return
	}
	approval, err := h.approvals.DecideToolApproval(request.Context(), store.DecideToolApprovalInput{
		TenantID: principal.TenantID, ApprovalID: id, ExpectedVersion: expectedVersion,
		Decision: decision, DecidedBy: body.DecidedBy, Now: time.Now().UTC(),
	})
	if err != nil {
		h.writeApprovalProblem(writer, request, err, traceID)
		return
	}
	h.writeApproval(writer, http.StatusOK, approval, traceID)
}

func (h *Handler) writeApproval(writer http.ResponseWriter, status int, approval store.ToolApproval, traceID string) {
	response := toolApprovalResponse{
		ID: approval.ID.String(), TenantID: approval.TenantID, CallID: approval.CallID.String(),
		TaskID: approval.TaskID.String(), RunID: approval.RunID.String(), AttemptID: approval.AttemptID.String(),
		ToolName: approval.ToolName, ToolVersion: approval.ToolVersion, Action: approval.Action,
		Resource: approval.Resource, ArgsHash: fmt.Sprintf("%x", approval.ArgsHash),
		Status: string(approval.Status), RequestedAt: approval.RequestedAt, ExpiresAt: approval.ExpiresAt,
		DecidedAt: approval.DecidedAt, DecidedBy: approval.DecidedBy,
		ResourceVersion: approval.ResourceVersion, TraceID: traceID,
	}
	writeJSON(writer, status, response)
}

func (h *Handler) writeApprovalProblem(writer http.ResponseWriter, request *http.Request, err error, traceID string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.writeProblem(writer, request, http.StatusNotFound, "APPROVAL_NOT_FOUND", "approval was not found", traceID)
	case errors.Is(err, store.ErrVersionConflict):
		h.writeProblem(writer, request, http.StatusConflict, "RESOURCE_VERSION_CONFLICT", "resource changed concurrently", traceID)
	case errors.Is(err, store.ErrInvalidTransition):
		h.writeProblem(writer, request, http.StatusConflict, "APPROVAL_STATE_CONFLICT", "approval is not pending", traceID)
	case errors.Is(err, store.ErrApprovalNotUsable):
		h.writeProblem(writer, request, http.StatusUnprocessableEntity, "APPROVAL_EXPIRED", "approval request has expired", traceID)
	default:
		h.writeProblem(writer, request, http.StatusInternalServerError, "INTERNAL_ERROR", "request could not be completed", traceID)
	}
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
		ctx := request.Context()
		if !strings.HasSuffix(request.URL.Path, "/events") {
			// Ordinary requests are bounded; event streams live until the
			// client disconnects or the task reaches a terminal phase.
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(request.Context(), requestTimeout)
			defer cancel()
		}
		traceID := request.Header.Get("X-Request-ID")
		if _, err := uuid.Parse(traceID); err != nil {
			traceID = h.newID().String()
		}
		writer.Header().Set("X-Request-ID", traceID)
		next.ServeHTTP(writer, request.WithContext(context.WithValue(ctx, traceKey{}, traceID)))
	})
}

func (h *Handler) writeTask(writer http.ResponseWriter, status int, task store.Task, usage usageSummary, budgetExhausted bool, traceID string) {
	response := taskResponseFrom(task, usage, budgetExhausted, traceID)
	writer.Header().Set("ETag", fmt.Sprintf(`W/"%d"`, task.ResourceVersion))
	writeJSON(writer, status, response)
}

func taskResponseFrom(task store.Task, usage usageSummary, budgetExhausted bool, traceID string) taskResponse {
	response := taskResponse{
		APIVersion: "agentos.dev/v1alpha1", Kind: "Task", ID: task.ID.String(),
		TenantID: task.TenantID, Namespace: task.Namespace, AgentVersionRef: task.AgentVersionRef,
		Goal: task.Goal, Spec: task.Spec, Phase: string(task.Phase), ResourceVersion: task.ResourceVersion,
		Usage: usage, BudgetExhausted: budgetExhausted,
		CreatedAt: task.CreatedAt.UTC(), UpdatedAt: task.UpdatedAt.UTC(), TraceID: traceID,
	}
	if task.AgentVersionID != nil {
		value := task.AgentVersionID.String()
		response.AgentVersionID = &value
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
	return response
}

func (h *Handler) writeAgentVersion(writer http.ResponseWriter, status int, version store.AgentVersion, traceID string) {
	response := agentVersionResponse{
		APIVersion: agentversion.APIVersion, Kind: agentversion.Kind, ID: version.ID.String(),
		TenantID: version.TenantID, Namespace: version.Namespace, Name: version.Name,
		Version: version.Version, Ref: version.Ref(), Spec: version.Spec,
		SpecDigest: fmt.Sprintf("%x", version.SpecDigest), ResourceVersion: version.ResourceVersion,
		CreatedAt: version.CreatedAt.UTC(), TraceID: traceID,
	}
	writer.Header().Set("ETag", fmt.Sprintf(`W/"%d"`, version.ResourceVersion))
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
	case errors.Is(err, store.ErrAgentVersionConflict):
		h.writeProblem(writer, request, http.StatusConflict, "AGENT_VERSION_CONFLICT", "agent version identity was already published with a different spec", traceID)
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
	if _, _, err := agentversion.ParseRef(body.AgentVersionRef); err != nil {
		return fmt.Errorf("agentVersionRef must be a canonical name@version reference: %v", err)
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

func validateCreateAgentVersion(body createAgentVersionRequest) error {
	if err := agentversion.ValidateName(body.Name); err != nil {
		return err
	}
	if err := agentversion.ValidateVersion(body.Version); err != nil {
		return err
	}
	if strings.TrimSpace(body.Namespace) == "" || len(body.Namespace) > 255 {
		return fmt.Errorf("namespace is required and must not exceed 255 bytes")
	}
	if err := agentversion.ValidateSpec(body.Spec); err != nil {
		return err
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
