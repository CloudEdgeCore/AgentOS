package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, client *http.Client) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("runtime interface base URL must be an absolute HTTP(S) URL")
	}
	if parsed.Hostname() == "" {
		return nil, errors.New("runtime interface base URL must include a host")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: parsed.String(), http: client}, nil
}

func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	var response HealthResponse
	err := c.call(ctx, http.MethodGet, "/v1alpha1/health", nil, http.StatusOK, &response)
	return response, err
}

func (c *Client) Start(ctx context.Context, request StartRequest) (StartResponse, error) {
	var response StartResponse
	err := c.call(ctx, http.MethodPost, "/v1alpha1/executions:start", request, http.StatusAccepted, &response)
	if httpError := new(HTTPError); errors.As(err, &httpError) && httpError.Status == http.StatusOK {
		if decodeErr := json.Unmarshal(httpError.Body, &response); decodeErr != nil {
			return StartResponse{}, decodeErr
		}
		err = nil
	}
	return response, err
}

func (c *Client) Stop(ctx context.Context, executionID string) (StopResponse, error) {
	var response StopResponse
	err := c.call(ctx, http.MethodPost, executionPath(executionID, ":stop"), struct{}{}, http.StatusAccepted, &response)
	return response, err
}

func (c *Client) Checkpoint(ctx context.Context, executionID string) (CheckpointResponse, error) {
	var response CheckpointResponse
	err := c.call(ctx, http.MethodPost, executionPath(executionID, ":checkpoint"), struct{}{}, http.StatusOK, &response)
	return response, err
}

func (c *Client) Restore(ctx context.Context, request RestoreRequest) (RestoreResponse, error) {
	var response RestoreResponse
	err := c.call(ctx, http.MethodPost, executionPath(request.ExecutionID, ":restore"), request, http.StatusOK, &response)
	return response, err
}

func (c *Client) Events(ctx context.Context, executionID string, after int64) (EventList, error) {
	var response EventList
	path := executionPath(executionID, "/events") + "?after=" + strconv.FormatInt(after, 10)
	err := c.call(ctx, http.MethodGet, path, nil, http.StatusOK, &response)
	return response, err
}

func (c *Client) Result(ctx context.Context, executionID string) (Result, bool, error) {
	var response Result
	err := c.call(ctx, http.MethodGet, executionPath(executionID, "/result"), nil, http.StatusOK, &response)
	if httpError := new(HTTPError); errors.As(err, &httpError) && httpError.Status == http.StatusAccepted {
		if decodeErr := json.Unmarshal(httpError.Body, &response); decodeErr != nil {
			return Result{}, false, decodeErr
		}
		return response, false, nil
	}
	return response, err == nil, err
}

func (c *Client) WaitResult(ctx context.Context, executionID string, interval time.Duration) (Result, error) {
	if interval <= 0 {
		interval = 25 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		result, terminal, err := c.Result(ctx, executionID)
		if err != nil {
			return Result{}, err
		}
		if terminal {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

type HTTPError struct {
	Status int
	Body   []byte
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("runtime interface returned HTTP %d: %s", e.Status, e.Body)
}

func (c *Client) call(ctx context.Context, method, path string, body any, expected int, response any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	reply, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer reply.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(reply.Body, maxInterfaceBody+1))
	if err != nil {
		return err
	}
	if len(encoded) > maxInterfaceBody {
		return errors.New("runtime interface response exceeds 2 MiB")
	}
	if reply.Header.Get("AgentOS-Runtime-Interface") != ProtocolVersion {
		return errors.New("runtime interface protocol negotiation failed")
	}
	if reply.StatusCode != expected {
		return &HTTPError{Status: reply.StatusCode, Body: encoded}
	}
	if response == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(response); err != nil {
		return fmt.Errorf("decode runtime response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("runtime interface response contains trailing JSON")
	}
	return nil
}

func executionPath(executionID, suffix string) string {
	return "/v1alpha1/executions/" + url.PathEscape(executionID) + suffix
}
