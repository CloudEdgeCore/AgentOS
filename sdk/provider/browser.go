package provider

import (
	"context"
	"encoding/json"
)

// NavigateRequest requests navigating the browser to a URL.
type NavigateRequest struct {
	URL            string `json:"url"`
	WaitUntil      string `json:"waitUntil,omitempty"` // load, domcontentloaded, networkidle
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

// ClickRequest requests clicking an element by CSS or XPath selector.
type ClickRequest struct {
	Selector string `json:"selector"`
	Timeout  int    `json:"timeout,omitempty"`
}

// EvaluateRequest executes JavaScript in the browser context.
type EvaluateRequest struct {
	Script string `json:"script"`
}

// ScreenshotRequest captures a page screenshot.
type ScreenshotRequest struct {
	FullPage bool   `json:"fullPage,omitempty"`
	Format   string `json:"format,omitempty"` // png, jpeg
	Quality  int    `json:"quality,omitempty"`
}

// BrowserResponse returns the page navigation or evaluation outcome.
type BrowserResponse struct {
	URL        string `json:"url"`
	StatusCode int    `json:"statusCode"`
	Title      string `json:"title,omitempty"`
	Content    string `json:"content,omitempty"`
}

// BrowserProvider supplies web browsing, automation, and DOM interaction capabilities.
type BrowserProvider interface {
	Provider
	Navigate(ctx context.Context, req NavigateRequest) (BrowserResponse, error)
	Screenshot(ctx context.Context, req ScreenshotRequest) ([]byte, error)
	Click(ctx context.Context, req ClickRequest) (BrowserResponse, error)
	Evaluate(ctx context.Context, req EvaluateRequest) (json.RawMessage, error)
}
