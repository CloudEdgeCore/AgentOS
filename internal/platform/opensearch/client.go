// Package opensearch is the minimal REST client for the OpenSearch projection
// target (ADR-013). Only the operations the projector needs are implemented:
// index management, document get/upsert/delete. No external SDK dependency:
// the projector talks to the OpenSearch HTTP API directly.
package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrNotFound reports a missing document or index.
	ErrNotFound = errors.New("opensearch document not found")
	// ErrUnavailable reports the cluster being unreachable or rejecting the
	// operation; callers retry with backoff.
	ErrUnavailable = errors.New("opensearch unavailable")
)

// Client is a minimal OpenSearch REST client.
type Client struct {
	addr       string
	user       string
	password   string
	httpClient *http.Client
	index      string
}

// Option configures the client.
type Option func(*Client)

// WithCredentials sets basic authentication (required when the security
// plugin is enabled).
func WithCredentials(user, password string) Option {
	return func(c *Client) { c.user, c.password = user, password }
}

// WithHTTPClient injects the HTTP client (tests and custom transports).
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) { c.httpClient = client }
}

// New connects to the OpenSearch cluster address, e.g. http://127.0.0.1:39200.
func New(addr, index string, options ...Option) (*Client, error) {
	if strings.TrimSpace(addr) == "" || strings.TrimSpace(index) == "" {
		return nil, fmt.Errorf("opensearch address and index are required")
	}
	client := &Client{
		addr:       strings.TrimRight(addr, "/"),
		index:      index,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
	for _, option := range options {
		option(client)
	}
	if client.httpClient == nil {
		client.httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return client, nil
}

// Ping checks the cluster health endpoint.
func (c *Client) Ping(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+"/_cluster/health", nil)
	if err != nil {
		return fmt.Errorf("opensearch health request: %w", err)
	}
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("%w: health check returned %s", ErrUnavailable, response.Status)
}

// EnsureIndex creates the index with the given mapping if it does not exist
// (idempotent: an existing index is left untouched).
func (c *Client) EnsureIndex(ctx context.Context, mapping []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, c.addr+"/"+c.index, bytes.NewReader(mapping))
	if err != nil {
		return fmt.Errorf("opensearch index request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	switch {
	case response.StatusCode == http.StatusOK || response.StatusCode == http.StatusBadRequest:
		// 200 = created; 400 with resource_already_exists_exception = exists.
		var reason struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if response.StatusCode == http.StatusBadRequest {
			_ = json.Unmarshal(body, &reason)
			if reason.Error.Type != "resource_already_exists_exception" {
				return fmt.Errorf("%w: create index: %s", ErrUnavailable, response.Status)
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: create index returned %s", ErrUnavailable, response.Status)
	}
}

// documentURL renders the REST path for a document. The id is URL-escaped so
// tenant-scoped ids ("tenant-a/<uuid>") survive as one path segment.
func (c *Client) documentURL(id string) string {
	return c.addr + "/" + c.index + "/_doc/" + url.PathEscape(id)
}

// Get returns the stored document for the id.
func (c *Client) Get(ctx context.Context, id string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.documentURL(id), nil)
	if err != nil {
		return nil, fmt.Errorf("opensearch get request: %w", err)
	}
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read document: %v", ErrUnavailable, err)
	}
	switch {
	case response.StatusCode == http.StatusOK:
		return body, nil
	case response.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("%w: get %s returned %s", ErrUnavailable, id, response.Status)
	}
}

// Index upserts the document under the id.
func (c *Client) Index(ctx context.Context, id string, document any) error {
	encoded, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("encode opensearch document: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, c.documentURL(id), bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("opensearch index request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%w: index %s returned %s", ErrUnavailable, id, response.Status)
	}
	return nil
}

// Delete removes the document; a missing document is idempotent success.
func (c *Client) Delete(ctx context.Context, id string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.documentURL(id), nil)
	if err != nil {
		return fmt.Errorf("opensearch delete request: %w", err)
	}
	c.authorize(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	if response.StatusCode == http.StatusNotFound {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%w: delete %s returned %s", ErrUnavailable, id, response.Status)
	}
	return nil
}

func (c *Client) authorize(request *http.Request) {
	if c.user != "" {
		request.SetBasicAuth(c.user, c.password)
	}
}
