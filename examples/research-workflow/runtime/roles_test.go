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
	// Render order must decide, never Go map iteration order: append a
	// later critic block and repeat — the final verdict has to win every
	// time regardless of hash randomization across processes.
	final := goal + "\n\nUpstream result [critic-final]:\n{\"status\":\"INSUFFICIENT_EVIDENCE\",\"score\":0.4}"
	for range 25 {
		if got := upstreamVerdict(final); got != "INSUFFICIENT_EVIDENCE" {
			t.Fatalf("upstreamVerdict after final = %q, want INSUFFICIENT_EVIDENCE", got)
		}
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

func TestCitationAcceptsExactGroundedQuote(t *testing.T) {
	bundles := []EvidenceBundle{{
		SourceID: "src-1",
		Claims: []Claim{
			{ClaimID: "src-1-claim-001", Evidence: "Runtimes schedule attempts with fencing tokens."},
		},
	}}
	raw, _ := json.Marshal(reportDoc{
		Title: "T", Summary: "S", Sections: []section{{Heading: "H", Body: "B"}},
		Citations: []citation{{Marker: "[1]", EvidenceID: "src-1-claim-001",
			Quote: "schedule attempts with   fencing tokens."}},
	})
	verdict := gradeCitations(raw, bundles)
	if !verdict.Valid || verdict.CitationCoverage != 1 || verdict.UnsupportedClaims != 0 {
		t.Fatalf("grounded quote must pass (whitespace normalized): %+v", verdict)
	}
}

func TestCitationRejectsEmptyQuote(t *testing.T) {
	bundles := []EvidenceBundle{{
		SourceID: "src-1",
		Claims:   []Claim{{ClaimID: "src-1-claim-001", Evidence: "Runtimes fence attempts."}},
	}}
	raw, _ := json.Marshal(reportDoc{
		Title: "T", Summary: "S", Sections: []section{{Heading: "H", Body: "B"}},
		Citations: []citation{{Marker: "[1]", EvidenceID: "src-1-claim-001", Quote: ""}},
	})
	verdict := gradeCitations(raw, bundles)
	if verdict.Valid || verdict.CitationCoverage != 0 || verdict.UnsupportedClaims != 1 {
		t.Fatalf("empty quote must be unsupported: %+v", verdict)
	}
}

func TestCitationRejectsUnknownEvidenceID(t *testing.T) {
	bundles := []EvidenceBundle{{
		SourceID: "src-1",
		// The quoted text exists in the corpus — but under a different id.
		Claims: []Claim{{ClaimID: "src-1-claim-001", Evidence: "Evidence memory is namespaced per workflow."}},
	}}
	raw, _ := json.Marshal(reportDoc{
		Title: "T", Summary: "S", Sections: []section{{Heading: "H", Body: "B"}},
		Citations: []citation{{Marker: "[1]", EvidenceID: "src-999-claim-042",
			Quote: "Evidence memory is namespaced"}},
	})
	verdict := gradeCitations(raw, bundles)
	if verdict.Valid || verdict.CitationCoverage != 0 || verdict.UnsupportedClaims != 1 {
		t.Fatalf("unknown evidenceId must be unsupported even if text matches elsewhere: %+v", verdict)
	}
}

func TestCitationRejectsQuoteFromDifferentEvidence(t *testing.T) {
	bundles := []EvidenceBundle{{
		SourceID: "src-1",
		Claims: []Claim{
			{ClaimID: "src-1-claim-001", Evidence: "Runtimes schedule attempts with fencing tokens."},
			{ClaimID: "src-1-claim-002", Evidence: "Evidence memory is namespaced per workflow."},
		},
	}}
	raw, _ := json.Marshal(reportDoc{
		Title: "T", Summary: "S", Sections: []section{{Heading: "H", Body: "B"}},
		Citations: []citation{{Marker: "[1]", EvidenceID: "src-1-claim-001",
			Quote: "namespaced per workflow"}},
	})
	verdict := gradeCitations(raw, bundles)
	if verdict.Valid || verdict.CitationCoverage != 0 || verdict.UnsupportedClaims != 1 {
		t.Fatalf("cross-evidence quote must be unsupported: %+v", verdict)
	}
}

func TestGradeCitationsMixedFabricationCounts(t *testing.T) {
	bundles := []EvidenceBundle{{
		SourceID: "src-1",
		Claims: []Claim{
			{ClaimID: "src-1-claim-001", Evidence: "Runtimes schedule attempts with fencing tokens."},
			{ClaimID: "src-1-claim-002", Evidence: "Evidence memory is namespaced per workflow."},
		},
	}}
	raw, _ := json.Marshal(reportDoc{
		Title: "T", Summary: "S", Sections: []section{{Heading: "H", Body: "B"}},
		Citations: []citation{
			{Marker: "[1]", EvidenceID: "src-1-claim-001", Quote: "fabricated quote that exists nowhere"},
			{Marker: "[2]", EvidenceID: "", Quote: "namespaced per workflow"},
		},
	})
	verdict := gradeCitations(raw, bundles)
	if verdict.Valid {
		t.Fatalf("fabricated quotes must fail validation: %+v", verdict)
	}
	if verdict.UnsupportedClaims != 2 || verdict.CitationCoverage != 0 {
		t.Fatalf("verdict = %+v", verdict)
	}
}

func TestGroundClaimsRejectsUngroundedEvidence(t *testing.T) {
	source := "The runtime fences every attempt. Recovery replays from checkpoints."
	claims := []Claim{
		{Claim: "c1", Evidence: "fences every attempt"},        // grounded
		{Claim: "c2", Evidence: "hallucinated passage absent"}, // rejected
		{Claim: "c3", Evidence: ""},                            // rejected
		{Claim: "c4", Evidence: "replays  from\ncheckpoints."}, // grounded after normalize
	}
	kept := groundClaims(claims, source)
	if len(kept) != 2 {
		t.Fatalf("kept = %+v", kept)
	}
	for _, claim := range kept {
		if !claim.Grounded || len(claim.SourceHash) != 64 {
			t.Fatalf("claim %s not stamped: %+v", claim.Claim, claim)
		}
	}
	if kept := groundClaims(claims[:2], "unrelated body"); len(kept) != 0 {
		t.Fatal("claims whose evidence misses the source must all be rejected")
	}
}

func TestApplyCriticTerminalRule(t *testing.T) {
	status := "NEEDS_MORE_RESEARCH"
	applyCriticTerminalRule(&status, 2)
	if status != "NEEDS_MORE_RESEARCH" {
		t.Fatalf("round 2 must keep the status: %q", status)
	}
	applyCriticTerminalRule(&status, 3)
	if status != "INSUFFICIENT_EVIDENCE" {
		t.Fatalf("round 3 non-PASS must become INSUFFICIENT_EVIDENCE: %q", status)
	}
	pass := "PASS"
	applyCriticTerminalRule(&pass, 3)
	if pass != "PASS" {
		t.Fatalf("an accepted analysis stays PASS: %q", pass)
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
