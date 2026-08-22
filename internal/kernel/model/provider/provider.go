// Package provider implements the real model execution layer behind the Model
// Gateway decision chain (v1.1 Phase 1.2): an OpenAI-compatible HTTP executor
// covering vLLM, Qwen, DeepSeek, GLM and any endpoint that speaks the
// /chat/completions dialect, with streaming, bounded retries, a per-provider
// circuit breaker, exact usage extraction and provider request-id capture.
//
// Credentials never cross this boundary in either direction: the API key is
// resolved from the configured environment variable per request, is attached
// only to the outbound Authorization header, and is stripped from every error
// message and telemetry surface.
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sentinel errors the invoker maps onto ledger outcomes.
var (
	// ErrProviderUnavailable reports a provider endpoint that could not be
	// reached after the bounded retry budget, or one whose circuit breaker
	// is open (fast fail).
	ErrProviderUnavailable = errors.New("model provider unavailable")
	// ErrProviderRejected reports a non-retryable provider refusal (4xx other
	// than 408/429): authentication, permission, malformed request or model.
	ErrProviderRejected = errors.New("model provider rejected the request")
	// ErrStreamAborted reports a stream that failed after content had already
	// been delivered; it is never retried, because a retry would duplicate
	// billed output.
	ErrStreamAborted = errors.New("model provider stream aborted after delivery")
)

// Message is one chat turn. Role is system|user|assistant|tool; ToolCalls
// carries OpenAI-style function calls on assistant turns; ToolCallID binds a
// tool-role turn to its call.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is one function call the model requested.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition declares one callable tool to the model.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Invocation is one provider request.
type Invocation struct {
	ModelName       string           `json:"model"`
	Messages        []Message        `json:"messages"`
	Temperature     *float64         `json:"temperature,omitempty"`
	MaxOutputTokens int32            `json:"max_tokens,omitempty"`
	Stream          bool             `json:"stream"`
	Tools           []ToolDefinition `json:"tools,omitempty"`
}

// Result is the outcome of one invocation. Usage is the exact provider-reported
// token counts (never estimated by the caller).
type Result struct {
	Content           string     `json:"content"`
	ToolCalls         []ToolCall `json:"toolCalls,omitempty"`
	FinishReason      string     `json:"finishReason"`
	InputTokens       int64      `json:"inputTokens"`
	OutputTokens      int64      `json:"outputTokens"`
	ProviderRequestID string     `json:"providerRequestId"`
}

// Config is one provider endpoint. APIKeyEnv names the environment variable
// the key is resolved from on every call; the key itself is never stored.
type Config struct {
	Name          string        `json:"name"`
	BaseURL       string        `json:"baseUrl"`
	APIKeyEnv     string        `json:"apiKeyEnv"`
	APIKey        string        `json:"-"` // programmatic (tests); file loads must use APIKeyEnv
	Timeout       time.Duration `json:"-"`
	TimeoutMs     int64         `json:"timeoutMs"`
	MaxAttempts   int           `json:"maxAttempts"`
	BreakerOpens  int           `json:"breakerOpens"`
	BreakerCoolMs int64         `json:"breakerCooldownMs"`
}

// Defaults applied by (Config).resolved().
const (
	defaultTimeout     = 120 * time.Second
	defaultMaxAttempts = 3
	defaultBreakerOpen = 5
	defaultBreakerCool = 30 * time.Second
	maxAttemptsCeiling = 8
	retryBaseDelay     = 250 * time.Millisecond
	retryMaxDelay      = 5 * time.Second
	maxRequestBytes    = 4 << 20
	maxResponseBytes   = 32 << 20
)

func (c Config) resolved() Config {
	if c.Timeout <= 0 {
		if c.TimeoutMs > 0 {
			c.Timeout = time.Duration(c.TimeoutMs) * time.Millisecond
		} else {
			c.Timeout = defaultTimeout
		}
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	if c.MaxAttempts > maxAttemptsCeiling {
		c.MaxAttempts = maxAttemptsCeiling
	}
	if c.BreakerOpens <= 0 {
		c.BreakerOpens = defaultBreakerOpen
	}
	if c.BreakerCoolMs <= 0 {
		c.BreakerCoolMs = int64(defaultBreakerCool / time.Millisecond)
	}
	return c
}

// Executor executes invocations against one OpenAI-compatible endpoint.
type Executor struct {
	config Config
	client *http.Client

	breakerMu       sync.Mutex
	consecutiveBad  int
	breakerOpenedAt time.Time
	probing         bool

	jitter func(time.Duration) time.Duration
	now    func() time.Time
}

// NewExecutor builds the executor for one provider endpoint.
func NewExecutor(config Config, client *http.Client) *Executor {
	if client == nil {
		client = &http.Client{}
	}
	return &Executor{
		config: config.resolved(), client: client,
		jitter: func(d time.Duration) time.Duration { return d/2 + time.Duration(rand.Int64N(int64(d)+1)) },
		now:    time.Now,
	}
}

// Name returns the provider name model references resolve against.
func (e *Executor) Name() string { return e.config.Name }

// Health probes the endpoint with a short-lived /models request. It is the
// half-open probe of the circuit breaker as well as the readiness signal.
func (e *Executor) Health(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := e.newRequest(probeCtx, http.MethodGet, "/models", nil)
	if err != nil {
		return err
	}
	response, err := e.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: health probe: %s", ErrProviderUnavailable, sanitize(err.Error(), e))
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("%w: health probe status %d", ErrProviderUnavailable, response.StatusCode)
	}
	return nil
}

// Complete performs one non-streaming invocation with bounded retries.
func (e *Executor) Complete(ctx context.Context, invocation Invocation) (Result, error) {
	invocation.Stream = false
	var result Result
	err := e.withRetries(ctx, invocation, func(ctx context.Context, request *http.Request) error {
		response, err := e.client.Do(request)
		if err != nil {
			return transportError(err, e)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
		if err != nil {
			return transportError(err, e)
		}
		if len(body) > maxResponseBytes {
			return &classifiedError{
				err:       fmt.Errorf("%w: response exceeds %d bytes", ErrProviderUnavailable, maxResponseBytes),
				retryable: true,
			}
		}
		if response.StatusCode/100 != 2 {
			return statusError(response, body, e)
		}
		parsed, err := decodeCompletion(body)
		if err != nil {
			return fmt.Errorf("%w: decode completion: %s", ErrProviderRejected, sanitize(err.Error(), e))
		}
		parsed.ProviderRequestID = requestID(response, parsed.rawID)
		result = parsed.Result
		return nil
	})
	return result, err
}

// Stream performs one streaming invocation, delivering content deltas to
// onDelta as they arrive. Retries happen only before the first delta is
// delivered; after that, a failure aborts the stream without retry so no
// output is duplicated.
func (e *Executor) Stream(ctx context.Context, invocation Invocation, onDelta func(string)) (Result, error) {
	invocation.Stream = true
	var result Result
	err := e.withRetries(ctx, invocation, func(ctx context.Context, request *http.Request) error {
		response, err := e.client.Do(request)
		if err != nil {
			return transportError(err, e)
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			return statusError(response, body, e)
		}
		providerRequestID := response.Header.Get("x-request-id")
		parsed, delivered, err := consumeStream(response.Body, onDelta)
		if err != nil {
			if delivered {
				return aborted(err, e)
			}
			return transportError(err, e)
		}
		parsed.ProviderRequestID = requestID(response, parsed.rawID)
		if providerRequestID != "" {
			parsed.ProviderRequestID = providerRequestID
		}
		result = parsed.Result
		return nil
	})
	return result, err
}

// classifiedError is the single retry classifier: Retryable says whether the
// attempt budget is consumed, RetryAfter carries the provider's hint.
type classifiedError struct {
	err        error
	retryable  bool
	retryAfter time.Duration
}

func (c *classifiedError) Error() string { return c.err.Error() }
func (c *classifiedError) Unwrap() error { return c.err }

func transportError(err error, e *Executor) error {
	return &classifiedError{
		err:       fmt.Errorf("%w: %s", ErrProviderUnavailable, sanitize(err.Error(), e)),
		retryable: true,
	}
}

func aborted(err error, e *Executor) error {
	return fmt.Errorf("%w: %s", ErrStreamAborted, sanitize(err.Error(), e))
}

func statusError(response *http.Response, body []byte, e *Executor) error {
	detail := sanitize(string(body), e)
	if len(detail) > 512 {
		detail = detail[:512]
	}
	statusCode := response.StatusCode
	classified := &classifiedError{retryAfter: parseRetryAfter(response)}
	if statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooManyRequests || statusCode >= 500 {
		classified.err = fmt.Errorf("%w: status %d: %s", ErrProviderUnavailable, statusCode, detail)
		classified.retryable = true
		return classified
	}
	classified.err = fmt.Errorf("%w: status %d: %s", ErrProviderRejected, statusCode, detail)
	return classified
}

// withRetries runs attempt under the circuit breaker with bounded exponential
// backoff. Only retryable errors (network, 408/429/5xx, malformed 2xx stream
// before delivery) consume the retry budget.
func (e *Executor) withRetries(ctx context.Context, invocation Invocation, attempt func(context.Context, *http.Request) error) error {
	if err := e.acquire(ctx); err != nil {
		return err
	}
	var lastErr error
	for try := 0; try < e.config.MaxAttempts; try++ {
		if try > 0 {
			if err := sleepBackoff(ctx, try, lastErr, e); err != nil {
				return err
			}
		}
		// Each attempt gets a fresh deadline; a slow provider consumes its
		// own budget, not the whole retry window.
		attemptCtx, cancel := context.WithTimeout(ctx, e.config.Timeout)
		request, err := e.newRequest(attemptCtx, http.MethodPost, "/chat/completions", encodeInvocation(invocation))
		if err != nil {
			cancel()
			return err
		}
		err = attempt(attemptCtx, request)
		cancel()
		if err == nil {
			e.recordSuccess()
			return nil
		}
		lastErr = err
		var classified *classifiedError
		if !errors.As(err, &classified) || !classified.retryable {
			e.recordFailure(false)
			return err
		}
		e.recordFailure(true)
	}
	return lastErr
}

func (e *Executor) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(e.config.BaseURL, "/")+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build provider request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if key := e.apiKey(); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	request.Header.Set("Accept", invocationAccept(body))
	return request, nil
}

func invocationAccept(body []byte) string {
	if bytes.Contains(body, []byte(`"stream":true`)) {
		return "text/event-stream"
	}
	return "application/json"
}

func (e *Executor) apiKey() string {
	if e.config.APIKey != "" {
		return e.config.APIKey
	}
	if e.config.APIKeyEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(e.config.APIKeyEnv))
}

// sleepBackoff waits before the next attempt, honoring Retry-After when the
// provider sent it on an HTTP status error.
func sleepBackoff(ctx context.Context, attempt int, cause error, e *Executor) error {
	delay := retryBaseDelay << (attempt - 1)
	if delay > retryMaxDelay {
		delay = retryMaxDelay
	}
	delay = e.jitter(delay)
	var classified *classifiedError
	if errors.As(cause, &classified) && classified.retryAfter > delay {
		delay = classified.retryAfter
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// parseRetryAfter extracts the Retry-After hint of an HTTP error response
// (delta-seconds or an HTTP date, bounded so a hostile value cannot stall
// the runtime).
func parseRetryAfter(response *http.Response) time.Duration {
	raw := strings.TrimSpace(response.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 && seconds <= 300 {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(raw); err == nil {
		if delay := time.Until(date); delay > 0 && delay <= 5*time.Minute {
			return delay
		}
	}
	return 0
}

// Circuit breaker: acquire admits the call (fast-failing while open), release
// is handled by recordSuccess/recordFailure through withRetries' defer-free
// flow. Half-open admits exactly one probe after the cooldown elapses.
func (e *Executor) acquire(ctx context.Context) error {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	if e.consecutiveBad < e.config.BreakerOpens {
		return nil
	}
	cooldown := time.Duration(e.config.BreakerCoolMs) * time.Millisecond
	if e.now().Sub(e.breakerOpenedAt) < cooldown {
		return fmt.Errorf("%w: circuit breaker open for provider %q", ErrProviderUnavailable, e.config.Name)
	}
	if e.probing {
		return fmt.Errorf("%w: circuit breaker half-open probe in flight", ErrProviderUnavailable)
	}
	e.probing = true
	return nil
}

func (e *Executor) recordSuccess() {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	e.consecutiveBad = 0
	e.probing = false
}

func (e *Executor) recordFailure(retryable bool) {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	if !retryable {
		// Definitive refusals say nothing about endpoint health.
		e.probing = false
		return
	}
	e.consecutiveBad++
	if e.consecutiveBad >= e.config.BreakerOpens {
		e.breakerOpenedAt = e.now()
	}
	e.probing = false
}

func encodeInvocation(invocation Invocation) []byte {
	type wire struct {
		Model         string           `json:"model"`
		Messages      []Message        `json:"messages"`
		Temperature   *float64         `json:"temperature,omitempty"`
		MaxTokens     int32            `json:"max_tokens,omitempty"`
		Stream        bool             `json:"stream,omitempty"`
		StreamOptions *wireStreamOpts  `json:"stream_options,omitempty"`
		Tools         []ToolDefinition `json:"tools,omitempty"`
	}
	value := wire{
		Model: invocation.ModelName, Messages: invocation.Messages,
		Temperature: invocation.Temperature, MaxTokens: invocation.MaxOutputTokens,
		Stream: invocation.Stream, Tools: invocation.Tools,
	}
	if invocation.Stream {
		value.StreamOptions = &wireStreamOpts{IncludeUsage: true}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	if len(encoded) > maxRequestBytes {
		return []byte(`{"error":"request exceeds size limit"}`)
	}
	return encoded
}

type wireStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []wireTool `json:"tool_calls"`
}

type wireCompletion struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int          `json:"index"`
		Message      *wireMessage `json:"message"`
		Delta        *wireMessage `json:"delta"`
		FinishReason *string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

type wireTool struct {
	ID       string `json:"id"`
	Index    *int   `json:"index"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type parsedCompletion struct {
	Result
	rawID string
}

func decodeCompletion(body []byte) (parsedCompletion, error) {
	var completion wireCompletion
	if err := json.Unmarshal(body, &completion); err != nil {
		return parsedCompletion{}, err
	}
	parsed := parsedCompletion{rawID: completion.ID}
	if len(completion.Choices) == 0 {
		return parsed, fmt.Errorf("completion has no choices")
	}
	choice := completion.Choices[0]
	if choice.Message != nil {
		parsed.Content = choice.Message.Content
		for _, call := range choice.Message.ToolCalls {
			parsed.ToolCalls = append(parsed.ToolCalls, ToolCall{
				ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments,
			})
		}
	}
	if choice.FinishReason != nil {
		parsed.FinishReason = *choice.FinishReason
	}
	if completion.Usage != nil {
		parsed.InputTokens = completion.Usage.PromptTokens
		parsed.OutputTokens = completion.Usage.CompletionTokens
	}
	return parsed, nil
}

// consumeStream reads one SSE stream. It returns the assembled result and
// whether any delta was delivered (retry safety).
func consumeStream(body io.Reader, onDelta func(string)) (parsed parsedCompletion, delivered bool, err error) {
	reader := bufio.NewReaderSize(body, 64<<10)
	var content strings.Builder
	toolCalls := map[int]*ToolCall{}
	var finishReason string
	var usage wireUsage
	haveUsage := false
	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed != "" {
			if data, ok := strings.CutPrefix(trimmed, "data:"); ok {
				payload := strings.TrimSpace(data)
				if payload == "[DONE]" {
					parsed.Content = content.String()
					for _, call := range toolCalls {
						parsed.ToolCalls = append(parsed.ToolCalls, *call)
					}
					parsed.FinishReason = finishReason
					if haveUsage {
						parsed.InputTokens, parsed.OutputTokens = usage.PromptTokens, usage.CompletionTokens
					}
					return parsed, delivered, nil
				}
				var chunk wireCompletion
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if chunk.ID != "" && parsed.rawID == "" {
						parsed.rawID = chunk.ID
					}
					if chunk.Usage != nil {
						usage = *chunk.Usage
						haveUsage = true
					}
					for _, choice := range chunk.Choices {
						if choice.FinishReason != nil && *choice.FinishReason != "" {
							finishReason = *choice.FinishReason
						}
						// OpenAI streams put increments in delta; some
						// compatible endpoints reuse message. Either is
						// accepted.
						increment := choice.Delta
						if increment == nil {
							increment = choice.Message
						}
						if increment == nil {
							continue
						}
						if increment.Content != "" {
							delivered = true
							if onDelta != nil {
								onDelta(increment.Content)
							}
							content.WriteString(increment.Content)
						}
						for _, call := range increment.ToolCalls {
							index := 0
							if call.Index != nil {
								index = *call.Index
							}
							existing := toolCalls[index]
							if existing == nil {
								existing = &ToolCall{}
								toolCalls[index] = existing
							}
							if call.ID != "" {
								existing.ID = call.ID
							}
							if call.Function.Name != "" {
								existing.Name = call.Function.Name
							}
							existing.Arguments += call.Function.Arguments
						}
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return parsed, delivered, fmt.Errorf("stream ended without a [DONE] sentinel")
			}
			return parsed, delivered, readErr
		}
	}
}

// requestID prefers the header, falls back to the response body id.
func requestID(response *http.Response, bodyID string) string {
	if header := strings.TrimSpace(response.Header.Get("x-request-id")); header != "" {
		return header
	}
	return bodyID
}

// sanitize removes credential material and endpoint URLs from provider error
// text before it crosses into kernel surfaces.
func sanitize(detail string, e *Executor) string {
	if key := e.apiKey(); key != "" {
		detail = strings.ReplaceAll(detail, key, "[redacted]")
	}
	detail = strings.ReplaceAll(detail, e.config.BaseURL, "[provider]")
	return detail
}
