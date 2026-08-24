package webtools

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sampleCorpus() []Document {
	return []Document{
		{
			SourceID: "src-runtime", Title: "Agent Runtime Infrastructure",
			URL: "https://docs.example.com/runtime", PublishedAt: "2026-01-01",
			Tags: []string{"runtime", "scheduling"}, Content: strings.Repeat("Runtime scheduling deep dive. ", 40),
		},
		{
			SourceID: "src-memory", Title: "Evidence Memory Design",
			URL: "https://docs.example.com/memory", PublishedAt: "2026-02-01",
			Tags: []string{"memory", "evidence"}, Content: "Short memory note.",
		},
	}
}

func post(t *testing.T, handler http.Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestServeHTTPRejectsNonPost(t *testing.T) {
	server := New(sampleCorpus())
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

func TestInvokeResourceDispatch(t *testing.T) {
	server := New(sampleCorpus())
	recorder := post(t, server, map[string]any{
		"action": "invoke", "resource": "web:search:*",
		"args": map[string]any{"query": "runtime scheduling"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("search status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var search struct {
		Results []SearchHit `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &search); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(search.Results) == 0 || search.Results[0].SourceID != "src-runtime" {
		t.Fatalf("unexpected results: %+v", search.Results)
	}

	recorder = post(t, server, map[string]any{
		"action": "invoke", "resource": "web:fetch:*",
		"args": map[string]any{"url": "https://docs.example.com/memory"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("fetch status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var fetch FetchResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &fetch); err != nil {
		t.Fatalf("decode fetch: %v", err)
	}
	if fetch.SourceID != "src-memory" || fetch.Content != "Short memory note." {
		t.Fatalf("unexpected fetch: %+v", fetch)
	}

	recorder = post(t, server, map[string]any{
		"action": "invoke", "resource": "web:delete:*", "args": map[string]any{},
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown resource status = %d, want 400", recorder.Code)
	}
}

func TestFetchPolicyBlocksUnsafeTargets(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		reason string
	}{
		{"file scheme", "file:///etc/passwd", "scheme"},
		{"credentials", "http://user:pass@example.com/doc", "credentials"},
		{"loopback name", "http://localhost/doc", "loopback"},
		{"loopback ip", "http://127.0.0.1:8080/doc", ""},
		{"private range", "http://10.1.2.3/doc", ""},
		{"link local", "http://169.254.169.254/latest/meta-data", ""},
		{"unspecified", "http://0.0.0.0/doc", ""},
		{"internal suffix", "http://metadata.internal/doc", "loopback"},
		{"bogus host", "http://definitely-not-a-host.invalid/doc", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := New(sampleCorpus())
			recorder := post(t, server, map[string]any{
				"action": "fetch", "args": map[string]any{"url": testCase.url},
			})
			if recorder.Code == http.StatusOK {
				t.Fatalf("%s was served: %s", testCase.url, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "blocked by fetch policy") &&
				!strings.Contains(recorder.Body.String(), "not in corpus") {
				t.Fatalf("unexpected rejection body: %s", recorder.Body.String())
			}
		})
	}
}

func TestSearchRankingAndLimit(t *testing.T) {
	server := New(sampleCorpus())
	recorder := post(t, server, map[string]any{
		"action": "search", "args": map[string]any{"query": "runtime memory", "limit": 1},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var response struct {
		Results []SearchHit `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("limit honored: %d hits", len(response.Results))
	}
	// The runtime document matches both tokens (title+content), the memory
	// document only one, so it must outrank despite alphabetical position.
	if response.Results[0].SourceID != "src-runtime" {
		t.Fatalf("top hit = %s, want src-runtime", response.Results[0].SourceID)
	}
	if len(response.Results[0].Snippet) > 220 {
		t.Fatalf("snippet not truncated: %d chars", len(response.Results[0].Snippet))
	}
}

func TestInjectFetchFailuresConsumesCount(t *testing.T) {
	server := New(sampleCorpus())
	server.InjectFetchFailures("example.com/memory", 1)
	for round := 0; round < 3; round++ {
		recorder := post(t, server, map[string]any{
			"action": "fetch", "args": map[string]any{"url": "https://docs.example.com/memory"},
		})
		switch round {
		case 0:
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("injected failure round %d: status %d", round, recorder.Code)
			}
		default:
			if recorder.Code != http.StatusOK {
				t.Fatalf("recovery round %d: status %d body=%s", round, recorder.Code, recorder.Body.String())
			}
		}
	}
}

func TestCountFetchesObserver(t *testing.T) {
	server := New(sampleCorpus())
	searches, fetches := 0, 0
	server.CountFetches(func(action string) {
		if action == "search" {
			searches++
		} else {
			fetches++
		}
	})
	post(t, server, map[string]any{"action": "search", "args": map[string]any{"query": "runtime"}})
	post(t, server, map[string]any{"action": "fetch", "args": map[string]any{"url": "https://docs.example.com/memory"}})
	if searches != 1 || fetches != 1 {
		t.Fatalf("observer counts searches=%d fetches=%d, want 1/1", searches, fetches)
	}
}
