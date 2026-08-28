// Package repository materializes the research domain model from kernel
// surfaces. It is a thin read-side client over the public Control API v1
// contract: workflows, tasks, and the operator memory search endpoint. The
// application layer never opens kernel internals or the database directly,
// so the same code serves the API server, the CLI, and the tests.
package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 2 << 20

// ErrNotFound reports a missing workflow, task, or record.
var ErrNotFound = fmt.Errorf("not found")

// ErrConflict reports an optimistic-concurrency rejection (409): the caller
// must re-read and retry with a fresh resource version.
var ErrConflict = fmt.Errorf("conflict")

// Client is one Control API v1 HTTP client.
type Client struct {
	endpoint   string
	httpClient *http.Client
	token      string
}

// NewClient builds a client. A nil httpClient installs sane defaults; a
// non-empty token is sent as a bearer token (production Control APIs).
func NewClient(endpoint string, httpClient *http.Client, token string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{endpoint: strings.TrimRight(endpoint, "/"), httpClient: httpClient, token: token}
}

// WorkflowStepView mirrors the workflow response's step objects
// (api/openapi/control-v1.yaml).
type WorkflowStepView struct {
	Name           string `json:"name"`
	Ordinal        int    `json:"ordinal"`
	Status         string `json:"status"`
	AttemptCount   int    `json:"attemptCount"`
	TaskID         string `json:"taskId,omitempty"`
	FailureCode    string `json:"failureCode,omitempty"`
	ParentStepName string `json:"parentStepName,omitempty"`
	IsDynamic      bool   `json:"isDynamic,omitempty"`
}

// WorkflowView mirrors the Control API workflow document.
type WorkflowView struct {
	ID              string             `json:"id"`
	Namespace       string             `json:"namespace"`
	Goal            string             `json:"goal"`
	Status          string             `json:"status"`
	ResourceVersion int64              `json:"resourceVersion"`
	FailureCode     string             `json:"failureCode,omitempty"`
	CreatedAt       time.Time          `json:"createdAt"`
	UpdatedAt       time.Time          `json:"updatedAt"`
	Deadline        *time.Time         `json:"deadline,omitempty"`
	Steps           []WorkflowStepView `json:"steps"`
}

// MemoryRecordView mirrors one Memory object of the memory search response.
type MemoryRecordView struct {
	ID          string `json:"id"`
	Namespace   string `json:"namespace"`
	Key         string `json:"key"`
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// TaskUsageView mirrors the cumulative task usage object.
type TaskUsageView struct {
	Tokens      int64   `json:"tokens"`
	CostUSD     float64 `json:"costUsd"`
	ToolCalls   int64   `json:"toolCalls"`
	WallSeconds int64   `json:"wallSeconds"`
}

// TaskView mirrors the Control API Task object (usage subset).
type TaskView struct {
	ID    string         `json:"id"`
	Phase string         `json:"phase"`
	Usage *TaskUsageView `json:"usage,omitempty"`
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, headers map[string]string, out any) error {
	target := c.endpoint + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("control api %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read control api response: %w", err)
	}
	if len(encoded) > maxResponseBytes {
		return fmt.Errorf("control api response exceeds %d bytes", maxResponseBytes)
	}
	switch {
	case response.StatusCode == 404:
		return ErrNotFound
	case response.StatusCode == 409:
		return fmt.Errorf("%w: %s", ErrConflict, responseDetail(encoded))
	case response.StatusCode >= 200 && response.StatusCode < 300:
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(encoded, out); err != nil {
			return fmt.Errorf("decode control api response: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("control api %s %s: %s (%s)", method, path, responseDetail(encoded), problemCode(encoded))
	}
}

func responseDetail(encoded []byte) string {
	var problem struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(encoded, &problem)
	detail := strings.TrimSpace(problem.Detail)
	if detail == "" {
		detail = strings.TrimSpace(problem.Title)
	}
	if detail == "" {
		return fmt.Sprintf("HTTP %s", strings.TrimSpace(string(encoded)))
	}
	return detail
}

func problemCode(encoded []byte) string {
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(encoded, &problem)
	return problem.Code
}

// CreateWorkflow publishes one workflow document (POST /v1/workflows).
func (c *Client) CreateWorkflow(ctx context.Context, namespace, goal string, document json.RawMessage, idempotencyKey string) (WorkflowView, error) {
	body, err := json.Marshal(struct {
		Namespace string          `json:"namespace,omitempty"`
		Goal      string          `json:"goal"`
		Workflow  json.RawMessage `json:"workflow"`
	}{Namespace: namespace, Goal: goal, Workflow: document})
	if err != nil {
		return WorkflowView{}, err
	}
	var workflow WorkflowView
	if err := c.do(ctx, http.MethodPost, "/v1/workflows", nil, strings.NewReader(string(body)),
		map[string]string{"Idempotency-Key": idempotencyKey}, &workflow); err != nil {
		return WorkflowView{}, err
	}
	return workflow, nil
}

// GetWorkflow reads one workflow document (GET /v1/workflows/{id}).
func (c *Client) GetWorkflow(ctx context.Context, workflowID string) (WorkflowView, error) {
	var workflow WorkflowView
	if err := c.do(ctx, http.MethodGet, "/v1/workflows/"+url.PathEscape(workflowID), nil, nil, nil, &workflow); err != nil {
		return WorkflowView{}, err
	}
	return workflow, nil
}

// CancelWorkflow requests cancellation with the required optimistic
// concurrency header (POST /v1/workflows/{id}/cancel).
func (c *Client) CancelWorkflow(ctx context.Context, workflowID string, resourceVersion int64) (WorkflowView, error) {
	var workflow WorkflowView
	if err := c.do(ctx, http.MethodPost, "/v1/workflows/"+url.PathEscape(workflowID)+"/cancel", nil, nil,
		map[string]string{"If-Match": fmt.Sprintf(`W/"%d"`, resourceVersion)}, &workflow); err != nil {
		return WorkflowView{}, err
	}
	return workflow, nil
}

// SearchMemories lists memory records of one namespace through the operator
// memory search endpoint. The query token follows the runtime's own search
// usage so the hybrid index reliably matches the record family.
func (c *Client) SearchMemories(ctx context.Context, namespace, query string, limit int) ([]MemoryRecordView, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	queryValues := url.Values{}
	queryValues.Set("namespace", namespace)
	if query != "" {
		queryValues.Set("query", query)
	}
	queryValues.Set("limit", strconv.Itoa(limit))
	var page struct {
		Memories []MemoryRecordView `json:"memories"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/memories", queryValues, nil, nil, &page); err != nil {
		return nil, err
	}
	return page.Memories, nil
}

// GetTask reads one Task object (GET /v1/tasks/{id}).
func (c *Client) GetTask(ctx context.Context, taskID string) (TaskView, error) {
	var task TaskView
	if err := c.do(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID), nil, nil, nil, &task); err != nil {
		return TaskView{}, err
	}
	return task, nil
}
