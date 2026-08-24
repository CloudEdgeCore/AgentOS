// Package webtools implements the two research tools of the reference
// workflow as a webhook-backed tool endpoint behind the AgentOS Tool
// Gateway:
//
//	web.search@1.0.0  query a fixed research corpus (offline deterministic)
//	web.fetch@1.0.0   fetch one corpus document body (SSRF-guarded)
//
// The v1 backend is an embedded corpus so the whole workflow runs without
// internet access and stays reproducible in CI; deployments swap in a real
// search engine by replacing the Backend while keeping the wire contract:
//
//	POST {"action":"invoke","resource":...,"args":{...}} -> 200 JSON result
package webtools

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// SearchHit is one ranked entry of a web.search response.
type SearchHit struct {
	SourceID    string `json:"sourceId"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	PublishedAt string `json:"publishedAt"`
	Snippet     string `json:"snippet"`
}

// FetchResult is the payload of a web.fetch response.
type FetchResult struct {
	SourceID  string `json:"sourceId"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Content   string `json:"content"`
	FetchedAt string `json:"fetchedAt"`
}

// Backend produces search hits and fetched documents behind the stable
// wire contract. The deterministic corpus implements it for CI; live
// deployments swap in real providers (Brave/Bing search, hardened HTTP
// fetch) without touching the agents or the workflow.
type Backend interface {
	Search(ctx context.Context, query string, limit int) ([]SearchHit, error)
	Fetch(ctx context.Context, target string) (*FetchResult, error)
}

// Server serves the tool contract over HTTP(S).
type Server struct {
	backend Backend
	// FailFetchesFor makes the next N fetches whose URL contains the token
	// fail with HTTP 500 (failure-injection seam used by the recovery and
	// tool-failure tests). Injection sits ABOVE the backend so it works for
	// deterministic and live providers alike.
	failTokens   map[string]int
	failureMu    chan struct{}
	requestCount func(method string)
}

// Document is one corpus entry.
type Document struct {
	SourceID    string   `json:"sourceId"`
	Title       string   `json:"title"`
	URL         string   `json:"url"`
	PublishedAt string   `json:"publishedAt"`
	Tags        []string `json:"tags"`
	Content     string   `json:"content"`
}

// New builds a Server over the given documents (deterministic backend).
func New(documents []Document) *Server {
	return &Server{
		backend:    newCorpusBackend(documents),
		failTokens: map[string]int{},
		failureMu:  make(chan struct{}, 1),
	}
}

// WithBackend replaces the backing provider (live web, enterprise search…).
func (s *Server) WithBackend(backend Backend) *Server {
	s.backend = backend
	return s
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var payload struct {
		Action   string          `json:"action"`
		Resource string          `json:"resource"`
		Args     json.RawMessage `json:"args"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid tool request: "+err.Error())
		return
	}
	if s.requestCount != nil {
		s.requestCount(payload.Action)
	}
	// The Tool Gateway posts {"action":"invoke","resource":"web:<verb>:*",...};
	// direct "search"/"fetch" actions are accepted for standalone use.
	verb := payload.Action
	if verb == "invoke" {
		switch {
		case strings.HasPrefix(payload.Resource, "web:search"):
			verb = "search"
		case strings.HasPrefix(payload.Resource, "web:fetch"):
			verb = "fetch"
		default:
			writeError(writer, http.StatusBadRequest, "unknown resource: "+payload.Resource)
			return
		}
	}
	switch verb {
	case "search":
		s.handleSearch(writer, request, payload.Args)
	case "fetch":
		s.handleFetch(writer, request, payload.Args)
	default:
		writeError(writer, http.StatusBadRequest, "unknown action: "+payload.Action)
	}
}

func (s *Server) handleSearch(writer http.ResponseWriter, request *http.Request, raw json.RawMessage) {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.Query) == "" {
		writeError(writer, http.StatusBadRequest, "query is required")
		return
	}
	if args.Limit <= 0 || args.Limit > 10 {
		args.Limit = 8
	}
	hits, err := s.backend.Search(request.Context(), args.Query, args.Limit)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "search backend: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"query": args.Query, "results": hits})
}

func (s *Server) handleFetch(writer http.ResponseWriter, request *http.Request, raw json.RawMessage) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.URL) == "" {
		writeError(writer, http.StatusBadRequest, "url is required")
		return
	}
	if s.shouldFailFetch(args.URL) {
		writeError(writer, http.StatusInternalServerError, "simulated upstream failure")
		return
	}
	result, err := s.backend.Fetch(request.Context(), args.URL)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "fetch backend: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

// corpusBackend is the deterministic offline provider: ranked search over
// the embedded corpus plus an allowlisted fetch of exactly those documents.
type corpusBackend struct {
	documents []Document
}

func newCorpusBackend(documents []Document) *corpusBackend {
	return &corpusBackend{documents: documents}
}

// Search ranks the corpus for the query tokens.
func (c *corpusBackend) Search(_ context.Context, query string, limit int) ([]SearchHit, error) {
	return rank(c.documents, tokenize(query), limit), nil
}

// Fetch serves one known document. The corpus is an explicit fetch
// allowlist: known documents are served offline (their hostnames do not
// resolve on purpose); anything else must pass the SSRF policy before we
// even attempt a lookup.
func (c *corpusBackend) Fetch(_ context.Context, target string) (*FetchResult, error) {
	for index := range c.documents {
		if sameDocument(c.documents[index].URL, target) {
			matched := &c.documents[index]
			return &FetchResult{
				SourceID: matched.SourceID, Title: matched.Title, URL: matched.URL,
				Content: matched.Content, FetchedAt: time.Now().UTC().Format(time.RFC3339),
			}, nil
		}
	}
	if reason := rejectUnsafeURL(target); reason != "" {
		return nil, fmt.Errorf("blocked by fetch policy: %s", reason)
	}
	return nil, fmt.Errorf("document not in corpus: %s", target)
}

// InjectFetchFailures fails the next count fetches matching token.
func (s *Server) InjectFetchFailures(token string, count int) {
	s.failureMu <- struct{}{}
	s.failTokens[token] += count
	<-s.failureMu
}

// CountFetches installs an observer invoked per tool call with its action
// ("fetch"/"search"); the failure-injection tests assert on fetch counts.
func (s *Server) CountFetches(observer func(action string)) {
	s.requestCount = observer
}

func (s *Server) shouldFailFetch(target string) bool {
	s.failureMu <- struct{}{}
	defer func() { <-s.failureMu }()
	for token, count := range s.failTokens {
		if count > 0 && strings.Contains(target, token) {
			s.failTokens[token] = count - 1
			return true
		}
	}
	return false
}

// rejectUnsafeURL is the SSRF boundary of web.fetch: absolute http(s) URLs
// only, never credentials, never non-public hosts. The v1 corpus additionally
// restricts every fetch to known document URLs, which turns the guard into an
// allowlist; deployments pointing at the live web keep the same checks.
func rejectUnsafeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "unparsable url"
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "scheme must be http or https (file:// and others are forbidden)"
	}
	if parsed.User != nil {
		return "credentials are forbidden"
	}
	host := parsed.Hostname()
	if host == "" {
		return "host is required"
	}
	if host == "localhost" || strings.EqualFold(host, "localhost.localdomain") || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return "loopback and internal hosts are forbidden"
	}
	addresses, err := net.LookupIP(host)
	if err != nil {
		// Unknown hosts cannot be resolved by the sandbox egress anyway;
		// treat them as blocked rather than leaking DNS failures.
		return "host did not resolve to a public address"
	}
	for _, address := range addresses {
		if isPrivate(address) {
			return "private network addresses are forbidden"
		}
	}
	return ""
}

func isPrivate(address net.IP) bool {
	return address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() ||
		address.IsLinkLocalMulticast() || address.IsUnspecified()
}

func tokenize(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	tokens := make([]string, 0, len(fields))
	for _, field := range fields {
		if len(field) >= 2 {
			tokens = append(tokens, field)
		}
	}
	return tokens
}

func rank(corpus []Document, tokens []string, limit int) []SearchHit {
	scored := make([]struct {
		document Document
		score    int
	}, 0, len(corpus))
	lower := func(text string) string { return strings.ToLower(text) }
	for _, document := range corpus {
		haystack := lower(document.Title) + "\n" + lower(strings.Join(document.Tags, " ")) + "\n" + lower(document.Content)
		score := 0
		title := lower(document.Title)
		for _, token := range tokens {
			if strings.Contains(title, token) {
				score += 3
			}
			if strings.Contains(haystack, token) {
				score++
			}
		}
		if score > 0 {
			scored = append(scored, struct {
				document Document
				score    int
			}{document, score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	hits := make([]SearchHit, 0, limit)
	for index, entry := range scored {
		if index >= limit {
			break
		}
		snippet := entry.document.Content
		if len(snippet) > 220 {
			snippet = snippet[:220]
		}
		hits = append(hits, SearchHit{
			SourceID: entry.document.SourceID, Title: entry.document.Title, URL: entry.document.URL,
			PublishedAt: entry.document.PublishedAt, Snippet: snippet,
		})
	}
	return hits
}

func sameDocument(a, b string) bool {
	return strings.TrimRight(strings.ToLower(a), "/") == strings.TrimRight(strings.ToLower(b), "/")
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"error": message})
}

// SelfSignedTLSListener serves handler on a loopback HTTPS listener whose
// certificate is trusted only by clients using the returned pool. The Tool
// Gateway requires https:// tool endpoints, so tests and local deployments
// use this helper instead of plaintext.
func SelfSignedTLSListener(handler http.Handler) (listener net.Listener, client *http.Client, endpoint string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generate key: %w", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "agentos-research-webtools"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("create certificate: %w", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("marshal key: %w", err)
	}
	certificate, err := tls.X509KeyPair(certificatePEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyPEM}))
	if err != nil {
		return nil, nil, "", fmt.Errorf("load key pair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificatePEM) {
		return nil, nil, "", errors.New("append certificate failed")
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, "", fmt.Errorf("listen: %w", err)
	}
	listener = tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{certificate}})
	client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	endpoint = "https://" + listener.Addr().String()
	return listener, client, endpoint, nil
}
