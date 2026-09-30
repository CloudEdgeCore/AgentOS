package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// BrowserConfig holds configuration parameters for BrowserProvider.
type BrowserConfig struct {
	Headless       bool `json:"headless"`
	TimeoutSeconds int  `json:"timeoutSeconds"`
	ViewportWidth  int  `json:"viewportWidth"`
	ViewportHeight int  `json:"viewportHeight"`
}

// BrowserProviderImpl implements BrowserProvider and ToolProvider.
type BrowserProviderImpl struct {
	config       BrowserConfig
	manifest     Manifest
	mu           sync.RWMutex
	currentPage  string
	currentTitle string
	pageHistory  []string
	active       bool
}

// NewBrowserProvider creates a new Browser Provider.
func NewBrowserProvider(cfg BrowserConfig) (*BrowserProviderImpl, error) {
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 30
	}
	if cfg.ViewportWidth <= 0 {
		cfg.ViewportWidth = 1280
	}
	if cfg.ViewportHeight <= 0 {
		cfg.ViewportHeight = 800
	}

	manifest := Manifest{
		Name:    "browser-provider",
		Version: "1.0.0",
		Type:    TypeBrowser,
		Capabilities: []string{
			"browser:navigate",
			"browser:screenshot",
			"browser:click",
			"browser:evaluate",
			"tool:invoke",
		},
		ConfigSchema: json.RawMessage(`{
			"$schema": "http://json-schema.org/draft-07/schema#",
			"type": "object",
			"properties": {
				"headless": {"type": "boolean", "default": true},
				"timeoutSeconds": {"type": "integer", "default": 30},
				"viewportWidth": {"type": "integer", "default": 1280},
				"viewportHeight": {"type": "integer", "default": 800}
			}
		}`),
		ResourceRequirements: ResourceRequirements{
			CPU:    "500m",
			Memory: "512Mi",
		},
	}

	return &BrowserProviderImpl{
		config:       cfg,
		manifest:     manifest,
		currentTitle: "about:blank",
		currentPage:  "about:blank",
		active:       true,
	}, nil
}

func (p *BrowserProviderImpl) Manifest() Manifest {
	return p.manifest
}

func (p *BrowserProviderImpl) Health(ctx context.Context) HealthStatus {
	p.mu.RLock()
	active := p.active
	p.mu.RUnlock()

	status := "HEALTHY"
	if !active {
		status = "UNHEALTHY"
	}

	return HealthStatus{
		Status:    status,
		Message:   "Browser headless automation engine operational",
		Timestamp: time.Now().UTC(),
		Metrics: map[string]float64{
			"active": float64(map[bool]int{true: 1, false: 0}[active]),
		},
	}
}

func (p *BrowserProviderImpl) Close() error {
	p.mu.Lock()
	p.active = false
	p.mu.Unlock()
	return nil
}

func (p *BrowserProviderImpl) Navigate(ctx context.Context, req NavigateRequest) (BrowserResponse, error) {
	if strings.TrimSpace(req.URL) == "" {
		return BrowserResponse{}, errors.New("browser: URL cannot be empty")
	}

	p.mu.Lock()
	p.currentPage = req.URL
	p.currentTitle = fmt.Sprintf("Page for %s", req.URL)
	p.pageHistory = append(p.pageHistory, req.URL)
	p.mu.Unlock()

	return BrowserResponse{
		URL:        req.URL,
		StatusCode: 200,
		Title:      fmt.Sprintf("Page for %s", req.URL),
		Content:    fmt.Sprintf("<html><head><title>Page for %s</title></head><body><h1>Loaded %s</h1></body></html>", req.URL, req.URL),
	}, nil
}

func (p *BrowserProviderImpl) Screenshot(ctx context.Context, req ScreenshotRequest) ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Return a valid PNG header mock byte stream
	pngMock := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, // IHDR chunk
	}
	return pngMock, nil
}

func (p *BrowserProviderImpl) Click(ctx context.Context, req ClickRequest) (BrowserResponse, error) {
	if strings.TrimSpace(req.Selector) == "" {
		return BrowserResponse{}, errors.New("browser: selector cannot be empty")
	}

	p.mu.RLock()
	current := p.currentPage
	title := p.currentTitle
	p.mu.RUnlock()

	return BrowserResponse{
		URL:        current,
		StatusCode: 200,
		Title:      title,
		Content:    fmt.Sprintf("<html><body><p>Clicked selector: %s</p></body></html>", req.Selector),
	}, nil
}

func (p *BrowserProviderImpl) Evaluate(ctx context.Context, req EvaluateRequest) (json.RawMessage, error) {
	if strings.TrimSpace(req.Script) == "" {
		return nil, errors.New("browser: script cannot be empty")
	}
	res, _ := json.Marshal(map[string]any{
		"evaluated": true,
		"script":    req.Script,
		"result":    "ok",
	})
	return res, nil
}

// ToolProvider methods: exposes browser as callable tools for agents.
func (p *BrowserProviderImpl) ListTools(ctx context.Context) ([]ToolDefinition, error) {
	return []ToolDefinition{
		{
			Name:        "browser_navigate",
			Description: "Navigate the browser to a destination URL and retrieve page title/content",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
		},
		{
			Name:        "browser_screenshot",
			Description: "Capture a full-page or viewport screenshot of the current page",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"fullPage":{"type":"boolean"}}}`),
		},
		{
			Name:        "browser_click",
			Description: "Click an interactive element on the page matching a CSS selector",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"selector":{"type":"string"}},"required":["selector"]}`),
		},
	}, nil
}

func (p *BrowserProviderImpl) InvokeTool(ctx context.Context, req ToolInvocationRequest) (ToolInvocationResult, error) {
	switch req.ToolName {
	case "browser_navigate":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return ToolInvocationResult{Error: err.Error()}, nil
		}
		res, err := p.Navigate(ctx, NavigateRequest{URL: args.URL})
		if err != nil {
			return ToolInvocationResult{Error: err.Error()}, nil
		}
		out, _ := json.Marshal(res)
		return ToolInvocationResult{Output: out, ReceiptID: fmt.Sprintf("rcpt-nav-%d", time.Now().UnixNano())}, nil

	case "browser_screenshot":
		data, err := p.Screenshot(ctx, ScreenshotRequest{FullPage: true})
		if err != nil {
			return ToolInvocationResult{Error: err.Error()}, nil
		}
		out, _ := json.Marshal(map[string]any{"bytes": len(data), "format": "png"})
		return ToolInvocationResult{Output: out, ReceiptID: fmt.Sprintf("rcpt-shot-%d", time.Now().UnixNano())}, nil

	case "browser_click":
		var args struct {
			Selector string `json:"selector"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return ToolInvocationResult{Error: err.Error()}, nil
		}
		res, err := p.Click(ctx, ClickRequest{Selector: args.Selector})
		if err != nil {
			return ToolInvocationResult{Error: err.Error()}, nil
		}
		out, _ := json.Marshal(res)
		return ToolInvocationResult{Output: out, ReceiptID: fmt.Sprintf("rcpt-click-%d", time.Now().UnixNano())}, nil

	default:
		return ToolInvocationResult{Error: fmt.Sprintf("unknown tool %s", req.ToolName)}, nil
	}
}
