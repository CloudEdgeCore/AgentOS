package webtools

// Tests for the live backends (roadmap P1). Everything runs against local
// httptest doubles or pure functions — no external network.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRejectUnsafeURLMatrix(t *testing.T) {
	cases := []struct {
		raw     string
		blocked bool
	}{
		{"http://169.254.169.254/latest/meta-data", true}, // link-local metadata
		{"http://10.0.0.9/internal", true},                // RFC1918
		{"http://192.168.1.4/router", true},               // RFC1918
		{"http://127.0.0.1:8080/admin", true},             // loopback
		{"http://localhost/secret", true},                 // loopback name
		{"file:///etc/passwd", true},                      // non-http scheme
		{"http://user:pass@example.com/", true},           // credentials
		{"ftp://example.com/file", true},                  // scheme
	}
	for _, testCase := range cases {
		reason := rejectUnsafeURL(testCase.raw)
		if testCase.blocked && reason == "" {
			t.Fatalf("%s must be blocked", testCase.raw)
		}
	}
}

func TestLiveRedirectPolicyLimitsHops(t *testing.T) {
	client := LiveHTTPClient()
	target, _ := http.NewRequest(http.MethodGet, "https://public.example.com/final", nil)
	via := make([]*http.Request, 0, liveMaxRedirects+2)
	for index := 0; index <= liveMaxRedirects; index++ {
		request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://public.example.com/hop%d", index), nil)
		via = append(via, request)
		if err := client.CheckRedirect(target, via[:index]); err != nil {
			t.Fatalf("hop %d rejected prematurely: %v", index, err)
		}
	}
	if err := client.CheckRedirect(target, via); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("expected redirect-limit error, got %v", err)
	}
}

func TestLiveRedirectPolicyBlocksPrivateHop(t *testing.T) {
	client := LiveHTTPClient()
	privateHop, _ := http.NewRequest(http.MethodGet, "http://10.1.2.3/escape", nil)
	first, _ := http.NewRequest(http.MethodGet, "https://public.example.com/start", nil)
	if err := client.CheckRedirect(privateHop, []*http.Request{first}); err == nil ||
		!strings.Contains(err.Error(), "fetch policy") {
		t.Fatalf("private redirect hop must be blocked, got %v", err)
	}
}

func TestClampBodyRejectsOversize(t *testing.T) {
	huge := strings.Repeat("x", liveMaxBodyBytes+16)
	if _, err := clampBody(strings.NewReader(huge)); err == nil {
		t.Fatal("oversize body must be rejected")
	}
	exact := strings.Repeat("x", liveMaxBodyBytes)
	body, err := clampBody(strings.NewReader(exact))
	if err != nil || len(body) != liveMaxBodyBytes {
		t.Fatalf("exact-size body must pass: %v %d", err, len(body))
	}
}

func TestExtractReadableDropsChromeAndKeepsTitle(t *testing.T) {
	page := `<!doctype html><html><head><title>Runtime Outlook</title>` +
		`<style>.x{color:red}</style></head><body>` +
		`<script>var tracking = 1;</script>` +
		`<h1>Heading</h1><p>First paragraph text.</p>` +
		`<div>Second block</div></body></html>`
	content, title := extractReadable([]byte(page), "text/html")
	if title != "Runtime Outlook" {
		t.Fatalf("title = %q", title)
	}
	if strings.Contains(content, "tracking") || strings.Contains(content, "color:red") {
		t.Fatalf("script/style leaked into content: %q", content)
	}
	for _, want := range []string{"Heading", "First paragraph text.", "Second block"} {
		if !strings.Contains(content, want) {
			t.Fatalf("content missing %q: %.200q", want, content)
		}
	}
	plain, plainTitle := extractReadable([]byte("raw body\nlines"), "text/plain")
	if plainTitle != "" || !strings.Contains(plain, "raw body") {
		t.Fatalf("plain passthrough broken: %q/%q", plain, plainTitle)
	}
}

func TestBraveSearchMapsResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Subscription-Token") != "test-key" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(writer, `{"web":{"results":[`+
			`{"title":"Agent runtimes","url":"https://a.example.com/x","description":"consolidation","age":"2 days ago"},`+
			`{"title":"No URL entry","url":"","description":"dropped"}]}}`)
	}))
	defer server.Close()
	search := &BraveSearch{APIKey: "test-key", Endpoint: server.URL, Client: server.Client()}
	hits, err := search.Search(context.Background(), "agent runtime", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].URL != "https://a.example.com/x" ||
		!strings.HasPrefix(hits[0].SourceID, liveSourceIDPrefix) {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestBingSearchMapsResultsAndAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Ocp-Apim-Subscription-Key") != "bing-key" {
			writer.WriteHeader(http.StatusForbidden)
			fmt.Fprint(writer, `{"error":"bad key"}`)
			return
		}
		fmt.Fprint(writer, `{"webPages":{"value":[`+
			`{"name":"Fencing tokens","url":"https://b.example.com/y","snippet":"lease renewal"}]}}`)
	}))
	defer server.Close()
	search := &BingSearch{APIKey: "wrong", Endpoint: server.URL, Client: server.Client()}
	if _, err := search.Search(context.Background(), "fencing", 5); err == nil {
		t.Fatal("auth failure must surface as error")
	}
	search.APIKey = "bing-key"
	hits, err := search.Search(context.Background(), "fencing", 5)
	if err != nil || len(hits) != 1 || hits[0].Snippet != "lease renewal" {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
}

func TestCompositeBackendDelegates(t *testing.T) {
	var seenQuery string
	composite := &CompositeBackend{
		SearchProvider: searchFunc(func(ctx context.Context, query string, limit int) ([]SearchHit, error) {
			seenQuery = query
			return []SearchHit{{SourceID: "s1"}}, nil
		}),
		FetchProvider: fetchFunc(func(ctx context.Context, target string) (*FetchResult, error) {
			return &FetchResult{SourceID: "f1"}, nil
		}),
	}
	if hits, _ := composite.Search(context.Background(), "hello", 3); len(hits) != 1 || seenQuery != "hello" {
		t.Fatalf("composite search delegation broken: %+v %q", hits, seenQuery)
	}
	if result, _ := composite.Fetch(context.Background(), "https://x"); result.SourceID != "f1" {
		t.Fatalf("composite fetch delegation broken: %+v", result)
	}
}

type searchFunc func(context.Context, string, int) ([]SearchHit, error)

func (f searchFunc) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	return f(ctx, query, limit)
}

type fetchFunc func(context.Context, string) (*FetchResult, error)

func (f fetchFunc) Fetch(ctx context.Context, target string) (*FetchResult, error) {
	return f(ctx, target)
}
