package research

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type modelMCPStub struct {
	args     map[string]any
	response json.RawMessage
}

func (stub *modelMCPStub) CallTool(_ context.Context, _ string, _ string, args any) (json.RawMessage, error) {
	stub.args, _ = args.(map[string]any)
	return stub.response, nil
}

func TestInvokeModelRequestsBoundedResearchCompletion(t *testing.T) {
	stub := &modelMCPStub{response: json.RawMessage(`{"content":"{}","finishReason":"stop"}`)}
	if _, err := InvokeModel(context.Background(), stub, "exec-1", "provider/model", []ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("invoke model: %v", err)
	}
	if got := stub.args["maxOutputTokens"]; got != researchMaxOutputTokens {
		t.Fatalf("maxOutputTokens = %v, want %d", got, researchMaxOutputTokens)
	}
}

func TestInvokeModelRejectsTruncatedCompletion(t *testing.T) {
	stub := &modelMCPStub{response: json.RawMessage(`{"content":"{","finishReason":"length"}`)}
	if _, err := InvokeModel(context.Background(), stub, "exec-1", "provider/model", []ChatMessage{{Role: "user", Content: "hi"}}); err == nil ||
		!strings.Contains(err.Error(), "truncated") {
		t.Fatalf("truncation error = %v", err)
	}
}

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

func TestDecodePlannerOutputNormalizesLiveModelAliases(t *testing.T) {
	plan, err := decodePlannerOutput(`{"questions":[` +
		`{"id":"model-id","text":"How do runtimes fence work?","priority":0,"queries":[" fencing tokens ",""]},` +
		`{"question":"How are budgets enforced?","searchQueries":[]}]}`)
	if err != nil {
		t.Fatalf("decode planner: %v", err)
	}
	if len(plan.Questions) != 2 || plan.Questions[0].ID != "rq-001" ||
		plan.Questions[0].Question != "How do runtimes fence work?" ||
		len(plan.Questions[0].SearchQueries) != 1 || plan.Questions[0].SearchQueries[0] != "fencing tokens" ||
		plan.Questions[1].SearchQueries[0] != plan.Questions[1].Question {
		t.Fatalf("normalized plan = %+v", plan)
	}
}

func TestDecodePlannerOutputRejectsSemanticallyEmptyQuestion(t *testing.T) {
	if _, err := decodePlannerOutput(`{"questions":[{"question":" ","searchQueries":[""]}]}`); err == nil ||
		!strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty question error = %v", err)
	}
}

func TestDecodeCriticOutputNormalizesLiveModelAliases(t *testing.T) {
	decision, err := decodeCriticOutput(`{"verdict":"needs_more_research","score":0.55,"gaps":[` +
		`{"description":"Which isolation mechanisms are used?","queries":[" sandbox isolation ",""]}]}`)
	if err != nil {
		t.Fatalf("decode critic: %v", err)
	}
	if decision.Status != "NEEDS_MORE_RESEARCH" || len(decision.Gaps) != 1 ||
		decision.Gaps[0].Question != "Which isolation mechanisms are used?" ||
		len(decision.Gaps[0].SuggestedQueries) != 1 || decision.Gaps[0].SuggestedQueries[0] != "sandbox isolation" {
		t.Fatalf("normalized decision = %+v", decision)
	}
}

func TestDecodeCriticOutputRejectsUnsupportedStatus(t *testing.T) {
	if _, err := decodeCriticOutput(`{"status":"MAYBE","score":0.9,"gaps":[]}`); err == nil ||
		!strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported status error = %v", err)
	}
}

func TestDecodeReportOutputNormalizesLiveModelAliases(t *testing.T) {
	report, err := decodeReportOutput(`{"report":{"reportTitle":"Runtime evolution","executiveSummary":"A governed transition.",` +
		`"sections":[{"title":"Control planes","content":"Fencing became explicit [1]."}],` +
		`"citations":[{"claimId":"src-007-claim-002","evidence":"Fencing became explicit."}]}}`)
	if err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.Title != "Runtime evolution" || report.Summary != "A governed transition." ||
		len(report.Sections) != 1 || report.Sections[0].Heading != "Control planes" ||
		len(report.Citations) != 1 || report.Citations[0].Marker != "[1]" ||
		report.Citations[0].EvidenceID != "src-007-claim-002" || report.Citations[0].SourceID != "src-007" {
		t.Fatalf("normalized report = %+v", report)
	}
}

func TestDecodeReportOutputRejectsIncompleteDocument(t *testing.T) {
	if _, err := decodeReportOutput(`{"title":"Only a title"}`); err == nil ||
		!strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete report error = %v", err)
	}
}

func TestDecodeReaderClaimsAcceptsStringAndObjectEvidence(t *testing.T) {
	claims, err := decodeReaderClaims(`{"claims":[` +
		`{"statement":"First","evidence":"verbatim one","confidence":0.8},` +
		`{"claim":"Second","evidence":{"quote":"verbatim two"},"confidence":0.9}]}`)
	if err != nil {
		t.Fatalf("decode reader claims: %v", err)
	}
	if len(claims) != 2 || claims[0].Claim != "First" || claims[0].Evidence != "verbatim one" ||
		claims[1].Claim != "Second" || claims[1].Evidence != "verbatim two" {
		t.Fatalf("normalized claims = %+v", claims)
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
