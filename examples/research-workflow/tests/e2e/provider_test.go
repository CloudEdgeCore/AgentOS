//go:build integration

package research_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
)

// scriptedProvider is an OpenAI-compatible chat completions endpoint that
// recognizes which research role is calling (via its system prompt) and
// emits deterministic, input-derived JSON - no external network involved.
type scriptedProvider struct {
	mu              sync.Mutex
	scenario        *scenario
	calls           int
	injectRemaining int
	injectStatus    int
}

// InjectHTTPFailures makes the next count provider calls fail with the given
// HTTP status (model-failure injection seam).
func (p *scriptedProvider) InjectHTTPFailures(status int, count int) {
	p.mu.Lock()
	p.injectStatus = status
	p.injectRemaining = count
	p.mu.Unlock()
}

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *scriptedProvider) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(os.Stderr, "[research-provider] panic: %v\n", recovered)
			writer.WriteHeader(http.StatusInternalServerError)
		}
	}()
	p.mu.Lock()
	p.calls++
	call := p.calls
	injected := p.injectRemaining > 0
	if injected {
		p.injectRemaining--
		status := p.injectStatus
		p.mu.Unlock()
		fmt.Fprintf(os.Stderr, "[research-provider] injected failure %d on call %d\n", status, call)
		http.Error(writer, "injected model failure", status)
		return
	}
	p.mu.Unlock()
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		fmt.Fprintf(os.Stderr, "[research-provider] decode: %v\n", err)
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	system, user := "", ""
	for _, message := range body.Messages {
		switch message.Role {
		case "system":
			system = message.Content
		case "user":
			user = message.Content
		}
	}
	content := p.render(system, user)
	writer.Header().Set("x-request-id", fmt.Sprintf("req-research-%d", call))
	if body.Stream {
		// Streaming mode: SSE chunks with role/content deltas then finish.
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flush := func() {
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		chunks := []map[string]any{
			{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}}}},
			{"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": content}}}},
			{"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
			{"choices": []map[string]any{}, "usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 90}},
		}
		for _, chunk := range chunks {
			encoded, _ := json.Marshal(chunk)
			fmt.Fprintf(writer, "data: %s\n\n", encoded)
			flush()
		}
		fmt.Fprint(writer, "data: [DONE]\n\n")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"id": fmt.Sprintf("chatcmpl-research-%d", call), "object": "chat.completion",
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 90},
	})
}

var (
	claimIDPattern  = regexp.MustCompile(`"claimId":"([a-z0-9-]+)"`)
	sourceIDPattern = regexp.MustCompile(`Document id:\s*(\S+)`)
	bodyMarker      = "Document body:"
)

func (p *scriptedProvider) render(system, user string) string {
	switch {
	case strings.Contains(system, "research planner"):
		return fmt.Sprintf(`{"questions":[` +
			`{"id":"rq-001","question":"Which control-plane architectures dominate agent runtime infrastructure?","priority":1,"searchQueries":["agent runtime control plane","agent infrastructure landscape"]},` +
			`{"id":"rq-002","question":"How do production platforms sandbox untrusted agent workloads?","priority":2,"searchQueries":["sandboxing agents gvisor firecracker wasm"]},` +
			`{"id":"rq-003","question":"How are budgets and durable recovery enforced across fan-out?","priority":3,"searchQueries":["fan-out economics budget","durable execution outbox"]}]}`)
	case strings.Contains(system, "extract verifiable factual claims"):
		return readerClaims(user)
	case strings.Contains(system, "research analyst"):
		return analystFindings(user)
	case strings.Contains(system, "research critic"):
		return p.criticDecision(user)
	case strings.Contains(system, "final research report"):
		return p.writerReport(user)
	default:
		return `{"error":"unknown role"}`
	}
}

func sentences(text string) []string {
	split := strings.Split(strings.ReplaceAll(text, "\n", " "), ". ")
	out := make([]string, 0, 6)
	for _, sentence := range split {
		sentence = strings.TrimSpace(sentence)
		if len(sentence) < 40 {
			continue
		}
		if !strings.HasSuffix(sentence, ".") {
			sentence += "."
		}
		out = append(out, sentence)
		if len(out) >= 3 {
			break
		}
	}
	return out
}

func readerClaims(user string) string {
	source := "src-000"
	if match := sourceIDPattern.FindStringSubmatch(user); match != nil {
		source = match[1]
	}
	bodyStart := strings.Index(user, bodyMarker)
	body := ""
	if bodyStart >= 0 {
		body = user[bodyStart+len(bodyMarker):]
	}
	claims := make([]string, 0, 3)
	for index, sentence := range sentences(body) {
		encoded, _ := json.Marshal(sentence)
		claims = append(claims, fmt.Sprintf(
			`{"claimId":"%s-claim-%03d","claim":%s,"evidence":%s,"confidence":0.82}`,
			source, index+1, encoded, encoded))
	}
	if len(claims) == 0 {
		encoded, _ := json.Marshal("The document discusses agent runtime infrastructure practices.")
		claims = append(claims, fmt.Sprintf(`{"claimId":"%s-claim-001","claim":%s,"evidence":%s,"confidence":0.6}`, source, encoded, encoded))
	}
	return `{"claims":[` + strings.Join(claims, ",") + `]}`
}

func analystFindings(user string) string {
	ids := claimIDPattern.FindAllStringSubmatch(user, -1)
	statements := map[string]string{
		"rq-001": "Control planes now own identity, capability and budget governance for agents.",
		"rq-002": "Sandbox choice is a cold-start economics decision as much as an isolation one.",
		"rq-003": "Budget reservation plus exact settlement keeps multi-agent cost curves bounded.",
	}
	findings := make([]string, 0, 3)
	for index := 0; index < 3 && index*2+1 < len(ids); index++ {
		statement := statements[map[int]string{0: "rq-001", 1: "rq-002", 2: "rq-003"}[index]]
		encodedStatement, _ := json.Marshal(statement)
		findings = append(findings, fmt.Sprintf(
			`{"findingId":"finding-%03d","statement":%s,"evidenceIds":["%s","%s"],"confidence":0.86}`,
			index+1, encodedStatement, ids[index*2][1], ids[index*2+1][1]))
	}
	if len(findings) == 0 && len(ids) > 0 {
		statement, _ := json.Marshal("Agent runtime consolidation around governed gateways continues.")
		findings = append(findings, fmt.Sprintf(
			`{"findingId":"finding-001","statement":%s,"evidenceIds":["%s"],"confidence":0.8}`, statement, ids[0][1]))
	}
	return `{"findings":[` + strings.Join(findings, ",") + `],"contradictions":[],"unknowns":[]}`
}

func (p *scriptedProvider) criticDecision(user string) string {
	round := 1
	if match := regexp.MustCompile(`Round:\s*(\d)`).FindStringSubmatch(user); match != nil {
		round = int(match[1][0] - '0')
	}
	p.mu.Lock()
	needsMore := (p.scenario.criticRound1NeedsMore && round == 1) ||
		p.scenario.criticAlwaysNeedsMore // every round; the runtime's terminal rule turns round 3 into INSUFFICIENT_EVIDENCE
	p.mu.Unlock()
	if needsMore {
		return `{"status":"NEEDS_MORE_RESEARCH","score":0.55,"gaps":[{"gapId":"gap-r1-001","question":"How is capacity federated across remote runtimes?","severity":"MEDIUM","suggestedQueries":["remote runtimes federation mtls"]}]}`
	}
	return `{"status":"PASS","score":0.91,"gaps":[]}`
}

// bundleClaims extracts the evidence-claim objects from every rendered
// "claims" array in the prompt, using real JSON semantics so quotes carry
// the exact stored text (regex capture keeps HTML escapes like \u0026 and
// breaks verbatim matching in the validator).
func bundleClaims(user string) []map[string]any {
	var claims []map[string]any
	const claimsKey = `"claims":[`
	rest := user
	for len(claims) < 4 {
		marker := strings.Index(rest, claimsKey)
		if marker < 0 {
			return claims
		}
		start := marker + len(claimsKey) - 1 // position of the opening bracket
		decoder := json.NewDecoder(strings.NewReader(rest[start:]))
		var items []any
		if err := decoder.Decode(&items); err != nil {
			rest = rest[start+1:]
			continue
		}
		for _, item := range items {
			if claim, ok := item.(map[string]any); ok {
				if _, has := claim["claimId"]; has && len(claims) < 4 {
					claims = append(claims, claim)
				}
			}
		}
		rest = rest[start+int(decoder.InputOffset()):]
	}
	return claims
}

func (p *scriptedProvider) writerReport(user string) string {
	p.mu.Lock()
	bad := p.scenario.writerBadCitations
	unknownEvidence := p.scenario.writerUnknownEvidence
	p.mu.Unlock()
	// A revision-aware model stops fabricating once the validator's
	// feedback reaches the prompt; the first draft stays dishonest.
	if bad && strings.Contains(user, "Revision") {
		bad = false
	}
	claims := bundleClaims(user)
	citations := make([]string, 0, len(claims))
	for index, claim := range claims {
		quote, _ := claim["evidence"].(string)
		source, _ := claim["sourceId"].(string)
		claimID, _ := claim["claimId"].(string)
		if bad {
			quote = "A fabricated passage that appears nowhere in the evidence base."
		}
		if unknownEvidence {
			// A persistently sloppy model: every citation points at a claim
			// id that does not exist in the evidence base. The hardened
			// validator must reject all of them on every revision.
			claimID = fmt.Sprintf("src-999-claim-%03d", index+7)
		}
		encodedQuote, _ := json.Marshal(quote)
		citations = append(citations, fmt.Sprintf(
			`{"marker":"[%d]","evidenceId":"%s","sourceId":"%s","quote":%s}`, index+1, claimID, source, encodedQuote))
	}
	if len(citations) == 0 {
		citations = append(citations, `{"marker":"[1]","evidenceId":"src-001-claim-001","sourceId":"src-001","quote":"Agent runtimes consolidated into control planes."}`)
	}
	insufficient := ""
	if strings.Contains(user, "INSUFFICIENT_EVIDENCE") {
		insufficient = `,"insufficientEvidence":true`
	}
	return fmt.Sprintf(`{"title":"Agent Runtime Infrastructure: Three-Year Outlook",`+
		`"summary":"Synthesis of twelve corpus sources covering control planes, sandboxing, budgets and federation.",`+
		`"sections":[{"heading":"Findings","body":"Consolidation around governed control planes accelerates."},`+
		`{"heading":"Outlook","body":"Expect capability attenuation and federated capacity to define the next cycle."}],`+
		`"citations":[%s]%s}`, strings.Join(citations, ","), insufficient)
}
