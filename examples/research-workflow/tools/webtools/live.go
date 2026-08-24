package webtools

// Live backends for the research tools (roadmap P1): real internet search
// via provider adapters and a hardened HTTP fetcher. The agents never see
// any of this — they keep calling web.search@1.0.0 / web.fetch@1.0.0.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	liveMaxRedirects   = 5
	liveMaxBodyBytes   = 10 << 20 // 10 MiB hard cap on fetched bodies
	liveFetchTimeout   = 20 * time.Second
	braveSearchAPI     = "https://api.search.brave.com/res/v1/web/search"
	bingSearchAPI      = "https://api.bing.microsoft.com/v7.0/search"
	liveUserAgent      = "AgentOS-Research/1.0 (+https://agentos.example/bot)"
	liveSourceIDPrefix = "live-"
)

// SearchProvider produces ranked hits for one query.
type SearchProvider interface {
	Search(context.Context, string, int) ([]SearchHit, error)
}

// FetchProvider downloads one target into a readable document.
type FetchProvider interface {
	Fetch(context.Context, string) (*FetchResult, error)
}

// CompositeBackend pairs one search provider with one fetch provider —
// the standard live assembly (e.g. BraveSearch + LiveFetch).
type CompositeBackend struct {
	SearchProvider SearchProvider
	FetchProvider  FetchProvider
}

// Search delegates to the search provider.
func (c *CompositeBackend) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	return c.SearchProvider.Search(ctx, query, limit)
}

// Fetch delegates to the fetch provider.
func (c *CompositeBackend) Fetch(ctx context.Context, target string) (*FetchResult, error) {
	return c.FetchProvider.Fetch(ctx, target)
}

// BraveSearch implements Backend.Search against the Brave Search API.
type BraveSearch struct {
	APIKey   string
	Endpoint string // overridable for tests; defaults to braveSearchAPI
	Client   *http.Client
}

// Search queries Brave and maps results to the unified hit shape.
func (b *BraveSearch) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	endpoint := b.Endpoint
	if endpoint == "" {
		endpoint = braveSearchAPI
	}
	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		endpoint+"?q="+url.QueryEscape(query)+"&count="+fmt.Sprint(min(limit, 20)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Subscription-Token", b.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", liveUserAgent)
	var document struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				Age         string `json:"age,omitempty"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := decodeLiveJSON(client, req, &document); err != nil {
		return nil, err
	}
	return mapLiveHits(document.Web.Results, func(item struct {
		Title       string `json:"title"`
		URL         string `json:"url"`
		Description string `json:"description"`
		Age         string `json:"age,omitempty"`
	}) (string, string, string) {
		return item.Title, item.URL, item.Description
	}), nil
}

// BingSearch implements Backend.Search against Azure Bing Search v7.
type BingSearch struct {
	APIKey   string
	Endpoint string
	Client   *http.Client
}

// Search queries Bing and maps results to the unified hit shape.
func (b *BingSearch) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	endpoint := b.Endpoint
	if endpoint == "" {
		endpoint = bingSearchAPI
	}
	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		endpoint+"?q="+url.QueryEscape(query)+"&count="+fmt.Sprint(min(limit, 20)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", b.APIKey)
	req.Header.Set("User-Agent", liveUserAgent)
	var document struct {
		WebPages struct {
			Value []struct {
				Name    string `json:"name"`
				URL     string `json:"url"`
				Snippet string `json:"snippet"`
			} `json:"value"`
		} `json:"webPages"`
	}
	if err := decodeLiveJSON(client, req, &document); err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(document.WebPages.Value))
	for _, item := range document.WebPages.Value {
		if item.URL == "" {
			continue
		}
		hits = append(hits, SearchHit{
			SourceID: liveSourceID(item.URL), Title: item.Name, URL: item.URL, Snippet: item.Snippet,
		})
	}
	return hits, nil
}

func mapLiveHits[T any](items []T, fields func(T) (string, string, string)) []SearchHit {
	hits := make([]SearchHit, 0, len(items))
	for _, item := range items {
		title, rawURL, snippet := fields(item)
		if rawURL == "" {
			continue
		}
		hits = append(hits, SearchHit{
			SourceID: liveSourceID(rawURL), Title: title, URL: rawURL, Snippet: snippet,
		})
	}
	return hits
}

func decodeLiveJSON(client *http.Client, request *http.Request, target any) error {
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("search api status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(io.LimitReader(response.Body, liveMaxBodyBytes)).Decode(target)
}

// liveSourceID derives a stable id from the URL so evidence records stay
// addressable across runs without central coordination.
func liveSourceID(rawURL string) string {
	digest := sha256.Sum256([]byte(rawURL))
	return liveSourceIDPrefix + hex.EncodeToString(digest[:])[:12]
}

// LiveFetch is the hardened real-web fetcher: SSRF policy, bounded
// redirects to public hosts only, size cap, timeout, content-type checks,
// HTML→readable-text extraction, final URL preserved.
type LiveFetch struct {
	Client *http.Client
}

// Fetch downloads one public document and returns readable text.
func (l *LiveFetch) Fetch(ctx context.Context, target string) (*FetchResult, error) {
	if reason := rejectUnsafeURL(target); reason != "" {
		return nil, fmt.Errorf("blocked by fetch policy: %s", reason)
	}
	client := l.Client
	if client == nil {
		client = LiveHTTPClient()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", liveUserAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", response.StatusCode)
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(
		response.Header.Get("Content-Type"), ";", 2)[0]))
	switch mediaType {
	case "text/html", "application/xhtml+xml", "text/plain", "application/json", "":
	default:
		return nil, fmt.Errorf("unsupported content type %q", mediaType)
	}
	body, err := clampBody(response.Body)
	if err != nil {
		return nil, err
	}
	finalURL := response.Request.URL.String()
	content, title := extractReadable(body, mediaType)
	return &FetchResult{
		SourceID: liveSourceID(finalURL), Title: title, URL: finalURL,
		Content: content, FetchedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// LiveHTTPClient returns the production fetch client: bounded redirects to
// public hosts only, request timeout, and the research user agent contract.
func LiveHTTPClient() *http.Client {
	return &http.Client{
		Timeout: liveFetchTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > liveMaxRedirects {
				return fmt.Errorf("more than %d redirects", liveMaxRedirects)
			}
			if reason := rejectUnsafeURL(request.URL.String()); reason != "" {
				return fmt.Errorf("redirect blocked by fetch policy: %s", reason)
			}
			return nil
		},
	}
}

// clampBody reads at most liveMaxBodyBytes from r and rejects larger
// payloads outright.
func clampBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, liveMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > liveMaxBodyBytes {
		return nil, fmt.Errorf("body exceeds %d bytes", liveMaxBodyBytes)
	}
	return body, nil
}

// extractReadable converts an HTML page into plain text (scripts/styles
// dropped, block structure kept as line breaks) or passes text/json through;
// it also pulls out the <title> when present.
func extractReadable(body []byte, mediaType string) (string, string) {
	if mediaType == "text/plain" || mediaType == "application/json" || mediaType == "" {
		return strings.TrimSpace(string(body)), ""
	}
	var text strings.Builder
	title := ""
	var walk func(*html.Node)
	skip := map[string]bool{"script": true, "style": true, "noscript": true, "svg": true}
	blockLevel := map[string]bool{
		"p": true, "div": true, "br": true, "li": true, "h1": true, "h2": true,
		"h3": true, "h4": true, "section": true, "article": true, "tr": true,
		"blockquote": true, "pre": true, "table": true,
	}
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			if skip[node.Data] {
				return
			}
			if node.Data == "title" && node.FirstChild != nil {
				title = strings.TrimSpace(node.FirstChild.Data)
			}
			if blockLevel[node.Data] && text.Len() > 0 {
				text.WriteString("\n")
			}
		}
		if node.Type == html.TextNode {
			line := strings.TrimSpace(node.Data)
			if line != "" {
				text.WriteString(line)
				text.WriteString(" ")
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if node.Type == html.ElementNode && blockLevel[node.Data] {
			text.WriteString("\n")
		}
	}
	if parsed, err := html.Parse(bytes.NewReader(body)); err == nil {
		walk(parsed)
	}
	lines := strings.Split(text.String(), "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			cleaned = append(cleaned, line)
		}
	}
	return strings.Join(cleaned, "\n"), title
}
