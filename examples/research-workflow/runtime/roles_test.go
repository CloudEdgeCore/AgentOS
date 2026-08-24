package research

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseEnvelopeToleratesTrailingUpstreamBlocks(t *testing.T) {
	goal := `AGENTOS-RESEARCH/v1 {"role":"reader","goal":"","workflowId":"wf-1","round":2,` +
		`"source":{"sourceId":"src-9","title":"T","url":"https://x.example.com/a"}}` +
		"\n\nUpstream result [critic-r1]:\n{\"status\":\"NEEDS_MORE_RESEARCH\"}"
	envelope := ParseEnvelope(goal, "research-reader@1.0.0")
	if envelope.Role != "reader" || envelope.Workflow != "wf-1" || envelope.Round != 2 {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.Source == nil || envelope.Source.SourceID != "src-9" {
		t.Fatalf("source = %+v", envelope.Source)
	}
}

func TestParseEnvelopeFallsBackToRoleFromRef(t *testing.T) {
	cases := map[string]string{
		"research-planner@1.0.0":        "planner",
		"research-citation-validator@1": "validator",
		"research-search@1.0.0":         "search",
		"research-collector@1.0.0":      "collector",
	}
	for ref, want := range cases {
		if got := RoleFromRef(ref); got != want {
			t.Fatalf("RoleFromRef(%s) = %s, want %s", ref, got, want)
		}
	}
	envelope := ParseEnvelope("plain goal without envelope", "research-writer@1.0.0")
	if envelope.Role != "writer" || envelope.Goal != "plain goal without envelope" {
		t.Fatalf("fallback envelope = %+v", envelope)
	}
}

func TestExtractUpstreamOutputsMultipleBlocks(t *testing.T) {
	goal := "prefix\n\nUpstream result [search-rq-001]:\n{\"sources\":[{\"url\":\"https://a\"}]}" +
		"\n\nUpstream result [critic-r1]:\n{\"status\":\"PASS\",\"score\":0.95}"
	outputs := ExtractUpstreamOutputs(goal)
	if len(outputs) != 2 {
		t.Fatalf("outputs = %v", outputs)
	}
	if !strings.Contains(outputs["search-rq-001"], "https://a") {
		t.Fatalf("search block = %q", outputs["search-rq-001"])
	}
	if got := upstreamVerdict(goal); got != "PASS" {
		t.Fatalf("upstreamVerdict = %q, want PASS", got)
	}
}

func TestFirstJSONObjectBalancedAndEscaped(t *testing.T) {
	text := `noise {"quote":"brace } inside \" quoted {ok}","nested":{"x":1}} trailing`
	got := firstJSONObject(text)
	var document struct {
		Quote  string `json:"quote"`
		Nested struct {
			X int `json:"x"`
		} `json:"nested"`
	}
	if err := json.Unmarshal([]byte(got), &document); err != nil {
		t.Fatalf("extracted %q: %v", got, err)
	}
	if document.Nested.X != 1 || !strings.Contains(document.Quote, "}") {
		t.Fatalf("document = %+v", document)
	}
	if firstJSONObject("no json here") != "" {
		t.Fatal("expected empty extraction")
	}
}

func TestGradeCitationsCoverageAndUnsupported(t *testing.T) {
	bundles := []EvidenceBundle{{
		SourceID: "src-1",
		Claims: []Claim{
			{ClaimID: "src-1-claim-001", Evidence: "Runtimes schedule attempts with fencing tokens."},
			{ClaimID: "src-1-claim-002", Evidence: "Evidence memory is namespaced per workflow."},
		},
	}}
	report := func(citations ...citation) json.RawMessage {
		raw, _ := json.Marshal(reportDoc{
			Title: "T", Summary: "S",
			Sections:  []section{{Heading: "H", Body: "B"}},
			Citations: citations,
		})
		return raw
	}

	verdict := gradeCitations(report(
		citation{Marker: "[1]", EvidenceID: "src-1-claim-001", Quote: "schedule attempts with   fencing tokens."},
	), bundles)
	if !verdict.Valid || verdict.CitationCoverage != 1 || verdict.UnsupportedClaims != 0 {
		t.Fatalf("supported verdict = %+v (whitespace must normalize)", verdict)
	}

	verdict = gradeCitations(report(
		citation{Marker: "[1]", EvidenceID: "src-1-claim-001", Quote: "fabricated quote that exists nowhere"},
		citation{Marker: "[2]", EvidenceID: "", Quote: "namespaced per workflow"},
	), bundles)
	if verdict.Valid {
		t.Fatalf("fabricated quote must fail validation: %+v", verdict)
	}
	if verdict.UnsupportedClaims != 1 || verdict.CitationCoverage != 0.5 {
		t.Fatalf("verdict = %+v", verdict)
	}

	// Unknown claim ids fall back to the whole-evidence haystack.
	verdict = gradeCitations(report(
		citation{Marker: "[1]", EvidenceID: "unknown-id", Quote: "Evidence memory is namespaced"},
	), bundles)
	if !verdict.Valid {
		t.Fatalf("haystack fallback verdict = %+v", verdict)
	}
}

func TestSeenDeduplicatesNormalizedURLs(t *testing.T) {
	byURL := map[string]SourceHit{}
	first := SourceHit{URL: "https://Example.com/doc/"}
	key := strings.TrimRight(strings.ToLower(first.URL), "/")
	byURL[key] = first
	if !seen(byURL, "https://example.com/doc") {
		t.Fatal("normalized variant of a stored url reported unseen")
	}
	if seen(byURL, "https://example.com/other") {
		t.Fatal("distinct url reported as seen")
	}
}

func TestEncodeEnvelopeRoundTrip(t *testing.T) {
	envelope := Envelope{
		Role: "reader", Workflow: "wf-7", Round: 3,
		Source: &SourceHit{SourceID: "src-2", URL: "https://x/y"},
	}
	parsed := ParseEnvelope(encodeEnvelope(envelope), "research-reader@1.0.0")
	if parsed.Role != "reader" || parsed.Workflow != "wf-7" || parsed.Round != 3 ||
		parsed.Source == nil || parsed.Source.SourceID != "src-2" {
		t.Fatalf("round trip mismatch: %+v", parsed)
	}
}
