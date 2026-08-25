package research

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Deps carries the brokered surfaces every role needs.
type Deps struct {
	MCP     MCPClient
	Models  Models
	Workdir func(namespace string) string // renders research/{workflow}/{leaf}
}

// Run executes one role turn and returns the step output document.
func Run(ctx context.Context, deps Deps, executionID, versionRef, goal string) (json.RawMessage, error) {
	envelope := ParseEnvelope(goal, versionRef)
	if envelope.Workflow == "" {
		return nil, fmt.Errorf("task envelope is missing workflowId: prefixMatch=%v raw=%.220q",
			strings.HasPrefix(goal, envelopePrefix), goal)
	}
	deps.Workdir = func(leaf string) string {
		return "research/" + envelope.Workflow + "/" + leaf
	}
	switch envelope.Role {
	case "planner":
		return runPlanner(ctx, deps, executionID, envelope)
	case "search":
		return runSearch(ctx, deps, executionID, envelope)
	case "collector":
		return runCollector(ctx, deps, executionID, envelope)
	case "reader":
		return runReader(ctx, deps, executionID, envelope)
	case "analyst":
		return runAnalyst(ctx, deps, executionID, envelope)
	case "critic":
		return runCritic(ctx, deps, executionID, envelope)
	case "writer":
		report, _, err := runWriter(ctx, deps, executionID, envelope, "")
		return report, err
	case "validator":
		return runValidator(ctx, deps, executionID, envelope)
	default:
		return nil, fmt.Errorf("unknown research role %q (ref %s)", envelope.Role, versionRef)
	}
}

func chat(ctx context.Context, deps Deps, executionID, system, user string) (string, error) {
	turn, err := InvokeModel(ctx, deps.MCP, executionID, deps.Models.Reasoning, []ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	})
	if err != nil {
		return "", err
	}
	return turn.Content, nil
}

const jsonOnlyInstruction = "Respond with ONE minified JSON document and nothing else. No markdown fences, no prose."

// -- planner -----------------------------------------------------------------

type plannerOutput struct {
	Questions []Question `json:"questions"`
}

// plannerWireQuestion accepts the stable field names plus the concise
// text/queries aliases commonly produced by OpenAI-compatible models. The
// normalized durable output always uses Question's stable schema.
type plannerWireQuestion struct {
	ID            string   `json:"id"`
	Question      string   `json:"question"`
	Text          string   `json:"text"`
	Priority      int      `json:"priority"`
	SearchQueries []string `json:"searchQueries"`
	Queries       []string `json:"queries"`
}

func decodePlannerOutput(content string) (plannerOutput, error) {
	var wire struct {
		Questions []plannerWireQuestion `json:"questions"`
	}
	if err := decodeLoose(content, &wire); err != nil {
		return plannerOutput{}, err
	}
	if len(wire.Questions) == 0 {
		return plannerOutput{}, fmt.Errorf("planner produced no questions")
	}
	if len(wire.Questions) > 6 {
		wire.Questions = wire.Questions[:6]
	}
	plan := plannerOutput{Questions: make([]Question, 0, len(wire.Questions))}
	for index, candidate := range wire.Questions {
		questionText := strings.TrimSpace(candidate.Question)
		if questionText == "" {
			questionText = strings.TrimSpace(candidate.Text)
		}
		if questionText == "" {
			return plannerOutput{}, fmt.Errorf("planner question %d is empty", index+1)
		}
		queries := candidate.SearchQueries
		if len(queries) == 0 {
			queries = candidate.Queries
		}
		cleanQueries := make([]string, 0, min(len(queries), 4))
		for _, query := range queries {
			if query = strings.TrimSpace(query); query != "" {
				cleanQueries = append(cleanQueries, query)
				if len(cleanQueries) == 4 {
					break
				}
			}
		}
		if len(cleanQueries) == 0 {
			cleanQueries = []string{questionText}
		}
		priority := candidate.Priority
		if priority <= 0 {
			priority = index + 1
		}
		plan.Questions = append(plan.Questions, Question{
			ID: fmt.Sprintf("rq-%03d", index+1), Question: questionText,
			Priority: priority, SearchQueries: cleanQueries,
		})
	}
	return plan, nil
}

func runPlanner(ctx context.Context, deps Deps, executionID string, envelope Envelope) (json.RawMessage, error) {
	system := "You are a research planner. Decompose the user's goal into at most 6 focused, non-overlapping research questions. " +
		"For each question provide 2-3 concrete web-search queries mixing broad and specific phrasing. " + jsonOnlyInstruction
	user := fmt.Sprintf("Research goal:\n%s\n\nSchema hint:\n%s", strings.TrimSpace(strings.TrimPrefix(envelope.Goal, envelopePrefix)),
		`{"questions":[{"id":"rq-001","question":"...","priority":1,"searchQueries":["...","..."]}]}`)
	content, err := chat(ctx, deps, executionID, system, user)
	if err != nil {
		return nil, err
	}
	plan, err := decodePlannerOutput(content)
	if err != nil {
		return nil, fmt.Errorf("planner output: %w", err)
	}
	if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("analysis"), "plan", "application/json", plan); err != nil {
		return nil, err
	}
	// Fan out one search probe per question; the workflow join group
	// "spawn:planner" closes once these children are terminal.
	for index := range plan.Questions {
		question := plan.Questions[index]
		childGoal := encodeEnvelope(Envelope{
			Role: "search", Workflow: envelope.Workflow, Round: envelope.Round, Question: &question,
		})
		if err := SpawnChild(ctx, deps.MCP, executionID,
			"search-"+question.ID, "research-search@1.0.0", childGoal, 3); err != nil {
			return nil, err
		}
	}
	output, _ := json.Marshal(plan)
	return output, nil
}

// -- search ------------------------------------------------------------------

type searchOutput struct {
	Question *Question   `json:"question"`
	Sources  []SourceHit `json:"sources"`
}

// searchSummary is intentionally compact. Dynamic child outputs are rendered
// into downstream step goals, whose Runtime Interface limit is 16 KiB. The
// complete source hits are already stored in durable memory, so returning
// snippets, titles, and URLs here only risks rejecting the downstream start.
type searchSummary struct {
	QuestionID  string   `json:"questionId"`
	SourceCount int      `json:"sourceCount"`
	SourceIDs   []string `json:"sourceIds"`
}

func runSearch(ctx context.Context, deps Deps, executionID string, envelope Envelope) (json.RawMessage, error) {
	if envelope.Question == nil {
		return nil, fmt.Errorf("search envelope carries no question")
	}
	question := envelope.Question
	seen := map[string]bool{}
	sources := make([]SourceHit, 0, 8)
	used := make([]string, 0, len(question.SearchQueries))
	for _, query := range question.SearchQueries {
		raw, err := CallTenantTool(ctx, deps.MCP, executionID, "web.search", map[string]any{"query": query, "limit": 6})
		if err != nil {
			continue // a failing query must not kill the whole probe
		}
		var page struct {
			Results []SourceHit `json:"results"`
		}
		if json.Unmarshal(raw, &page) != nil {
			continue
		}
		used = append(used, query)
		for _, hit := range page.Results {
			key := strings.TrimRight(strings.ToLower(hit.URL), "/")
			if seen[key] {
				continue
			}
			seen[key] = true
			hit.Query = query
			hit.ResearchQuestionID = question.ID
			hit.QuestionText = question.Question
			sources = append(sources, hit)
			if len(sources) >= 8 {
				break
			}
		}
		if len(sources) >= 8 {
			break
		}
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("search found no sources for %s (%s)", question.ID, strings.Join(used, "; "))
	}
	if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("sources"), "found-"+question.ID, "application/json", sources); err != nil {
		return nil, err
	}
	// Seed the evidence namespace with snippet-level bundles so round-one
	// analysis has an evidence base before any reader runs; reader passes
	// later replace these with full verbatim-claim bundles. The snippet IS
	// the retrieved source text at this point, so its claims are grounded
	// against it verbatim and hashed accordingly.
	for index, hit := range sources {
		bundle := EvidenceBundle{
			SourceID:           hit.SourceID,
			ResearchQuestionID: question.ID,
			Title:              hit.Title,
			URL:                hit.URL,
			Claims: []Claim{{
				ClaimID:    fmt.Sprintf("%s-claim-%03d", hit.SourceID, index+1),
				Claim:      hit.Title,
				Evidence:   hit.Snippet,
				Confidence: 0.5,
				SourceHash: sourceHash(hit.Snippet),
				Grounded:   true,
			}},
		}
		if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("evidence"), "ev-"+hit.SourceID, "application/json", bundle); err != nil {
			return nil, err
		}
	}
	sourceIDs := make([]string, 0, len(sources))
	for _, source := range sources {
		sourceIDs = append(sourceIDs, source.SourceID)
	}
	output, _ := json.Marshal(searchSummary{
		QuestionID: question.ID, SourceCount: len(sources), SourceIDs: sourceIDs,
	})
	return output, nil
}

// -- collector ---------------------------------------------------------------

type collectorOutput struct {
	SpawnedReaders []string `json:"spawnedReaders"`
	Skipped        []string `json:"skipped"`
	Deferred       []string `json:"deferred,omitempty"`
}

// maxReadersPerDrain mirrors the workflow template's runtime.dynamic
// maxChildrenPerStep guard. Exceeding it would fail the collector step, and
// a failed spawn parent leaves its join group permanently open — so the
// collector defers surplus sources to a later round instead.
const (
	maxReadersPerDrain         = 8
	readerMaxTransientFailures = 3 // kept equal to spawned Reader maxAttempts
)

// runCollector drains every source discovered by its dependency group and
// spawns one reader task per unread source. It performs no model call: the
// role exists because the kernel's spawn-group join covers DIRECT children,
// so a dedicated routing hop turns a finished search wave into a bounded,
// joinable reader wave (documented deviation from the draft document).
func runCollector(ctx context.Context, deps Deps, executionID string, envelope Envelope) (json.RawMessage, error) {
	discovered := map[string]SourceHit{}
	order := make([]string, 0, 16)
	for _, raw := range ExtractUpstreamOutputs(envelope.Goal) {
		var candidates []searchOutput
		if err := json.Unmarshal([]byte(raw), &candidates); err != nil {
			candidates = nil
		}
		if candidates == nil {
			var single searchOutput
			if err := json.Unmarshal([]byte(raw), &single); err != nil || single.Sources == nil {
				continue
			}
			candidates = []searchOutput{single}
		}
		for _, entry := range candidates {
			for _, hit := range entry.Sources {
				key := strings.TrimRight(strings.ToLower(hit.URL), "/")
				if key == "" || seen(discovered, key) {
					continue
				}
				discovered[key] = hit
				order = append(order, key)
			}
		}
	}
	// Later rounds discover sources from the Evidence Memory tree: the
	// spawn-group join renders the critic's verdict, not the round-one
	// search outputs, so the sources namespace is the durable bus. The
	// store's hybrid search needs a keyword present in every record, and
	// "sourceId" is the JSON field name shared by all source hits.
	sourceRecords, err := SearchMemory(ctx, deps.MCP, executionID, deps.Workdir("sources"), "sourceId", 100)
	if err != nil {
		return nil, fmt.Errorf("collector source scan: %w", err)
	}
	for _, record := range sourceRecords {
		var hits []SourceHit
		if json.Unmarshal([]byte(record.Content), &hits) != nil {
			continue
		}
		for _, hit := range hits {
			key := strings.TrimRight(strings.ToLower(hit.URL), "/")
			if key == "" || seen(discovered, key) {
				continue
			}
			discovered[key] = hit
			order = append(order, key)
		}
	}
	sort.Strings(order)
	result := collectorOutput{SpawnedReaders: []string{}, Skipped: []string{}}
	// One exact-key scan decides what still needs reading: "sourceId" is shared
	// by grounded bundles and audited skip markers, so both outcomes prevent a
	// failed or redirected source from being spawned again in a later round.
	readKeys := map[string]bool{}
	evidenceRecords, err := SearchMemory(ctx, deps.MCP, executionID, deps.Workdir("evidence"), "sourceId", 100)
	if err != nil {
		return nil, fmt.Errorf("collector evidence scan: %w", err)
	}
	for _, record := range evidenceRecords {
		readKeys[record.Key] = true
	}
	for index, key := range order {
		if index >= 16 {
			break // hard scan wall per collector round
		}
		hit := discovered[key]
		if readKeys["read-"+hit.SourceID] {
			result.Skipped = append(result.Skipped, hit.SourceID)
			continue
		}
		if len(result.SpawnedReaders) >= maxReadersPerDrain {
			result.Deferred = append(result.Deferred, hit.SourceID)
			continue
		}
		name := "reader-" + hit.SourceID
		childGoal := encodeEnvelope(Envelope{
			Role: "reader", Workflow: envelope.Workflow, Round: envelope.Round, Source: &hit,
		})
		if err := SpawnChild(ctx, deps.MCP, executionID, name, "research-reader@1.0.0", childGoal, readerMaxTransientFailures); err != nil {
			return nil, err
		}
		result.SpawnedReaders = append(result.SpawnedReaders, name)
	}
	if len(result.SpawnedReaders) == 0 && len(result.Skipped) == 0 {
		// An empty drain is legitimate convergence: every discovered source
		// is already read (or the critic spawned no new probes), so the
		// join proceeds vacuously.
		result.SpawnedReaders = []string{}
		result.Skipped = []string{}
	}
	output, _ := json.Marshal(result)
	return output, nil
}

func seen(byURL map[string]SourceHit, key string) bool {
	_, ok := byURL[key]
	return ok
}

// -- reader ------------------------------------------------------------------

type Claim struct {
	ClaimID    string  `json:"claimId"`
	Claim      string  `json:"claim"`
	Evidence   string  `json:"evidence"`
	Confidence float64 `json:"confidence"`
	// SourceHash pins the exact retrieved source text the evidence was
	// verified against (sha256 hex); Grounded records that verification.
	SourceHash string `json:"sourceHash,omitempty"`
	Grounded   bool   `json:"grounded"`
}

type EvidenceBundle struct {
	SourceID           string  `json:"sourceId"`
	ResearchQuestionID string  `json:"researchQuestionId"`
	Title              string  `json:"title"`
	URL                string  `json:"url"`
	Claims             []Claim `json:"claims"`
}

// readerSummary keeps dependency rendering bounded. The full grounded bundle
// is written to evidence memory before this receipt is returned.
type readerSummary struct {
	SourceID    string   `json:"sourceId"`
	ClaimCount  int      `json:"claimCount"`
	EvidenceIDs []string `json:"evidenceIds"`
	Skipped     bool     `json:"skipped,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}

type readerWireClaim struct {
	Claim      string          `json:"claim"`
	Statement  string          `json:"statement"`
	Evidence   json.RawMessage `json:"evidence"`
	Quote      string          `json:"quote"`
	Confidence float64         `json:"confidence"`
}

func decodeReaderClaims(content string) ([]Claim, error) {
	var wire struct {
		Claims []readerWireClaim `json:"claims"`
	}
	if err := decodeLoose(content, &wire); err != nil {
		return nil, err
	}
	claims := make([]Claim, 0, len(wire.Claims))
	for _, candidate := range wire.Claims {
		statement := strings.TrimSpace(candidate.Claim)
		if statement == "" {
			statement = strings.TrimSpace(candidate.Statement)
		}
		evidence := strings.TrimSpace(candidate.Quote)
		if evidence == "" && len(candidate.Evidence) > 0 {
			_ = json.Unmarshal(candidate.Evidence, &evidence)
			if evidence == "" {
				var object struct {
					Quote   string `json:"quote"`
					Text    string `json:"text"`
					Passage string `json:"passage"`
				}
				if json.Unmarshal(candidate.Evidence, &object) == nil {
					evidence = strings.TrimSpace(object.Quote)
					if evidence == "" {
						evidence = strings.TrimSpace(object.Text)
					}
					if evidence == "" {
						evidence = strings.TrimSpace(object.Passage)
					}
				}
			}
		}
		if statement != "" && evidence != "" {
			claims = append(claims, Claim{Claim: statement, Evidence: evidence, Confidence: candidate.Confidence})
		}
	}
	return claims, nil
}

func skipReader(ctx context.Context, deps Deps, executionID string, source *SourceHit, cause error) (json.RawMessage, error) {
	reason := truncate(strings.TrimSpace(cause.Error()), 240)
	marker := readerSummary{SourceID: source.SourceID, EvidenceIDs: []string{}, Skipped: true, Reason: reason}
	if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("evidence"), "read-"+source.SourceID, "application/json", marker); err != nil {
		return nil, err
	}
	output, _ := json.Marshal(marker)
	return output, nil
}

type readerRetryState struct {
	SourceID    string `json:"sourceId"`
	Failures    int    `json:"failures"`
	LastReason  string `json:"lastReason"`
	Disposition string `json:"disposition"`
}

// retryReader records a non-terminal failure without writing read-<source>.
// On the final configured attempt it degrades explicitly to an audited SKIP,
// preventing one permanently unreachable URL from poisoning the dynamic
// Reader join while retaining proof that retries were exhausted.
func retryReader(ctx context.Context, deps Deps, executionID string, source *SourceHit, label string, cause error) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("reader %s interrupted: %w", label, cause)
	}
	key := "retry-" + source.SourceID
	records, err := SearchMemory(ctx, deps.MCP, executionID, deps.Workdir("evidence"), source.SourceID, 20)
	if err != nil {
		return nil, fmt.Errorf("load Reader retry state: %w", err)
	}
	state := readerRetryState{SourceID: source.SourceID}
	for _, record := range records {
		if record.Key == key {
			_ = json.Unmarshal([]byte(record.Content), &state)
			break
		}
	}
	state.SourceID = source.SourceID
	state.Failures++
	state.LastReason = truncate(strings.TrimSpace(cause.Error()), 240)
	state.Disposition = "RETRY"
	if state.Failures >= readerMaxTransientFailures {
		state.Disposition = "SKIP_RETRY_EXHAUSTED"
	}
	if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("evidence"), key, "application/json", state); err != nil {
		return nil, fmt.Errorf("persist Reader retry state: %w", err)
	}
	if state.Failures >= readerMaxTransientFailures {
		return skipReader(ctx, deps, executionID, source,
			fmt.Errorf("%s retry exhausted after %d attempts: %w", label, state.Failures, cause))
	}
	return nil, fmt.Errorf("reader retryable %s failure %d/%d: %w",
		label, state.Failures, readerMaxTransientFailures, cause)
}

type ReaderErrorDisposition int

const (
	ReaderRetry ReaderErrorDisposition = iota
	ReaderSkip
	ReaderFail
)

// classifyReaderError is deliberately conservative: an unknown transport or
// provider failure must not become a permanent "read" marker. Only explicit,
// deterministic source outcomes are safe to skip.
func classifyReaderError(err error) ReaderErrorDisposition {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ReaderRetry
	}
	var outcome *MCPToolError
	if !errors.As(err, &outcome) {
		return ReaderRetry
	}
	if strings.HasPrefix(outcome.Code, "TOOL_ENDPOINT_HTTP_") {
		statusCode, parseErr := strconv.Atoi(strings.TrimPrefix(outcome.Code, "TOOL_ENDPOINT_HTTP_"))
		if parseErr != nil {
			return ReaderFail
		}
		switch statusCode {
		case http.StatusNotFound, http.StatusGone, http.StatusUnsupportedMediaType,
			http.StatusBadRequest, http.StatusUnprocessableEntity:
			return ReaderSkip
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
			return ReaderRetry
		}
		if statusCode >= 500 && statusCode <= 599 {
			return ReaderRetry
		}
		return ReaderFail
	}
	switch outcome.Code {
	case "PROVIDER_UNAVAILABLE", "RESOURCE_VERSION_CONFLICT", "SPAWN_RECONCILIATION_PENDING":
		return ReaderRetry
	case "PROVIDER_REJECTED", "POLICY_DENIED", "BUDGET_EXHAUSTED":
		return ReaderFail
	default:
		return ReaderRetry
	}
}

func handleReaderFetchError(ctx context.Context, deps Deps, executionID string, source *SourceHit, err error) (json.RawMessage, error) {
	switch classifyReaderError(err) {
	case ReaderSkip:
		return skipReader(ctx, deps, executionID, source, err)
	case ReaderFail:
		return nil, fmt.Errorf("reader terminal fetch failure: %w", err)
	default:
		return retryReader(ctx, deps, executionID, source, "fetch", err)
	}
}

func runReader(ctx context.Context, deps Deps, executionID string, envelope Envelope) (json.RawMessage, error) {
	if envelope.Source == nil {
		return nil, fmt.Errorf("reader envelope carries no source")
	}
	source := envelope.Source
	fetched, err := CallTenantTool(ctx, deps.MCP, executionID, "web.fetch", map[string]any{"url": source.URL})
	if err != nil {
		return handleReaderFetchError(ctx, deps, executionID, source, err)
	}
	var document FetchedDocument
	if err := json.Unmarshal(fetched, &document); err != nil {
		return retryReader(ctx, deps, executionID, source, "fetch decode", err)
	}
	if strings.TrimSpace(document.SourceID) == "" || strings.TrimSpace(document.URL) == "" || strings.TrimSpace(document.Content) == "" {
		return retryReader(ctx, deps, executionID, source, "fetch contract", fmt.Errorf("fetched document is incomplete"))
	}
	system := "You extract verifiable factual claims from a source document for a research pipeline. " +
		"Extract 2-6 claims most relevant to the research question. Evidence MUST be one exact, contiguous, verbatim passage copied from the document body. " +
		"Use only the exact field names in the requested schema. " + jsonOnlyInstruction
	user := fmt.Sprintf("Research question (%s):\n%s\n\nDocument id: %s\nDocument title: %s\nDocument body:\n%s\n\nReturn schema:\n%s",
		source.ResearchQuestionID, source.QuestionText, document.SourceID, document.Title, document.Content,
		`{"claims":[{"claim":"...","evidence":"exact verbatim passage","confidence":0.87}]}`)
	content, err := InvokeModelText(ctx, deps, executionID, deps.Models.Reader, system, user)
	if err != nil {
		return retryReader(ctx, deps, executionID, source, "model", err)
	}
	claims, err := decodeReaderClaims(content)
	if err != nil {
		return retryReader(ctx, deps, executionID, source, "model output", err)
	}
	// Evidence → Original Source grounding (roadmap §3.4): every claim's
	// evidence must appear verbatim in the fetched document text; claims
	// that fail are rejected here, before they can reach memory.
	bundle := EvidenceBundle{
		SourceID: document.SourceID, ResearchQuestionID: source.ResearchQuestionID,
		Title: document.Title, URL: document.URL,
		Claims: groundClaims(claims, document.Content),
	}
	for index := range bundle.Claims {
		bundle.Claims[index].ClaimID = fmt.Sprintf("%s-claim-%03d", bundle.SourceID, index+1)
		if bundle.Claims[index].Confidence <= 0 {
			bundle.Claims[index].Confidence = 0.5
		}
	}
	if len(bundle.Claims) > 6 {
		bundle.Claims = bundle.Claims[:6]
	}
	if len(bundle.Claims) == 0 {
		return skipReader(ctx, deps, executionID, source,
			fmt.Errorf("reader produced no grounded claims for %s", bundle.SourceID))
	}
	// "ev-" carries the (upgraded) evidence bundle; "read-" is the marker the
	// collector checks to avoid re-spawning a completed source.
	if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("evidence"), "ev-"+bundle.SourceID, "application/json", bundle); err != nil {
		return nil, err
	}
	if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("evidence"), "read-"+source.SourceID, "application/json", bundle); err != nil {
		return nil, err
	}
	evidenceIDs := make([]string, 0, len(bundle.Claims))
	for _, claim := range bundle.Claims {
		evidenceIDs = append(evidenceIDs, claim.ClaimID)
	}
	output, _ := json.Marshal(readerSummary{
		SourceID: bundle.SourceID, ClaimCount: len(bundle.Claims), EvidenceIDs: evidenceIDs,
	})
	return output, nil
}

// groundClaims filters model-extracted claims down to the ones whose
// evidence text appears verbatim (whitespace-normalized) in the retrieved
// source text, stamping each survivor with the source content hash. It is
// the Evidence → Original Source verification step of the provenance chain.
func groundClaims(claims []Claim, sourceText string) []Claim {
	hash := sourceHash(sourceText)
	haystack := normalizeSpace(sourceText)
	grounded := make([]Claim, 0, len(claims))
	for _, claim := range claims {
		evidence := normalizeSpace(claim.Evidence)
		if evidence == "" || !strings.Contains(haystack, evidence) {
			continue // reject the claim: its "evidence" is not in the source
		}
		claim.SourceHash = hash
		claim.Grounded = true
		grounded = append(grounded, claim)
	}
	return grounded
}

func sourceHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

func InvokeModelText(ctx context.Context, deps Deps, executionID, modelRef, system, user string) (string, error) {
	turn, err := InvokeModel(ctx, deps.MCP, executionID, modelRef, []ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	})
	if err != nil {
		return "", err
	}
	return turn.Content, nil
}

// upstreamVerdict scans the rendered dependency outputs for a critic
// decision and returns its status ("PASS", "NEEDS_MORE_RESEARCH",
// "INSUFFICIENT_EVIDENCE", ""). Blocks are scanned in RENDER ORDER — never
// map order — so the LAST critic block (the final round) always wins.
func upstreamVerdict(goal string) string {
	verdict := ""
	const marker = "\n\nUpstream result ["
	cursor := goal
	for {
		start := strings.Index(cursor, marker)
		if start < 0 {
			return verdict
		}
		rest := cursor[start+len(marker):]
		end := strings.Index(rest, "]:\n")
		if end < 0 {
			return verdict
		}
		name := rest[:end]
		body := rest[end+3:]
		next := strings.Index(body, marker)
		block := body
		if next >= 0 {
			block = body[:next]
			cursor = body[next:]
		}
		var probe struct {
			Status string `json:"status"`
		}
		if name == "" || json.Unmarshal([]byte(firstJSONObject(block)), &probe) != nil {
			continue
		}
		switch probe.Status {
		case "PASS", "NEEDS_MORE_RESEARCH", "INSUFFICIENT_EVIDENCE":
			verdict = probe.Status // the latest critic in render order speaks last
		}
		if next < 0 {
			return verdict
		}
	}
}

// -- analyst -----------------------------------------------------------------

type analysisDoc struct {
	Findings []struct {
		FindingID   string   `json:"findingId"`
		Statement   string   `json:"statement"`
		EvidenceIDs []string `json:"evidenceIds"`
		Confidence  float64  `json:"confidence"`
	} `json:"findings"`
	Contradictions []struct {
		Topic       string   `json:"topic"`
		EvidenceIDs []string `json:"evidenceIds"`
	} `json:"contradictions,omitempty"`
	Unknowns    []string `json:"unknowns,omitempty"`
	Passthrough bool     `json:"passthrough,omitempty"`
}

func runAnalyst(ctx context.Context, deps Deps, executionID string, envelope Envelope) (json.RawMessage, error) {
	previous := fmt.Sprintf("analysis-r%d", envelope.Round-1)
	current := fmt.Sprintf("analysis-r%d", maxInt(envelope.Round, 1))
	// A carried PASS verdict means the evidence base did not change: replay
	// the stored analysis instead of paying for another model pass.
	if upstreamVerdict(envelope.Goal) == "PASS" && envelope.Round > 1 {
		if stored, ok, _ := readAnalysis(ctx, deps, executionID, previous); ok {
			stored.Passthrough = true
			output, _ := json.Marshal(stored)
			if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("analysis"), current, "application/json", stored); err != nil {
				return nil, err
			}
			return output, nil
		}
	}
	bundles := collectEvidence(ctx, deps, executionID, envelope)
	if len(bundles) == 0 {
		return nil, fmt.Errorf("analyst %s found no evidence bundles", current)
	}
	system := "You are a research analyst. Cluster the extracted claims into findings supported by explicit evidence IDs, " +
		"list contradictions between sources and open unknowns. Keep at most 12 findings. " + jsonOnlyInstruction
	user := "Evidence bundles:\n" + renderBundles(bundles) +
		fmt.Sprintf("\n\nReturn schema:\n%s", `{"findings":[{"findingId":"finding-001","statement":"...","evidenceIds":["src-001-claim-001"],"confidence":0.87}],"contradictions":[{"topic":"...","evidenceIds":["a","b"]}],"unknowns":["..."]}`)
	content, err := chat(ctx, deps, executionID, system, truncate(user, 24000))
	if err != nil {
		return nil, err
	}
	var analysis analysisDoc
	if err := decodeLoose(content, &analysis); err != nil {
		return nil, fmt.Errorf("analyst output: %w", err)
	}
	for index := range analysis.Findings {
		analysis.Findings[index].FindingID = fmt.Sprintf("finding-%03d", index+1)
		if len(analysis.Findings[index].EvidenceIDs) == 0 {
			analysis.Findings[index].EvidenceIDs = []string{bundles[0].Claims[0].ClaimID}
		}
	}
	if len(analysis.Findings) == 0 {
		return nil, fmt.Errorf("analyst produced no findings")
	}
	analysis.Passthrough = false
	if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("analysis"), current, "application/json", analysis); err != nil {
		return nil, err
	}
	output, _ := json.Marshal(analysis)
	return output, nil
}

func readAnalysis(ctx context.Context, deps Deps, executionID, key string) (analysisDoc, bool, error) {
	records, err := SearchMemory(ctx, deps.MCP, executionID, deps.Workdir("analysis"), key, 10)
	if err != nil {
		return analysisDoc{}, false, err
	}
	for _, record := range records {
		if record.Key != key {
			continue
		}
		var stored analysisDoc
		if json.Unmarshal([]byte(record.Content), &stored) == nil {
			return stored, true, nil
		}
	}
	return analysisDoc{}, false, nil
}

func collectEvidence(ctx context.Context, deps Deps, executionID string, envelope Envelope) []EvidenceBundle {
	bundles := map[string]EvidenceBundle{}
	appendBundle := func(raw string) {
		trimmed := firstJSONObject(raw)
		var bundle EvidenceBundle
		if trimmed == "" || json.Unmarshal([]byte(trimmed), &bundle) != nil ||
			bundle.SourceID == "" || len(bundle.Claims) == 0 {
			return
		}
		if _, exists := bundles[bundle.SourceID]; !exists {
			bundles[bundle.SourceID] = bundle
		}
	}
	for _, raw := range ExtractUpstreamOutputs(envelope.Goal) {
		appendBundle(raw)
	}
	if len(bundles) < 3 {
		records, err := SearchMemory(ctx, deps.MCP, executionID, deps.Workdir("evidence"), "claims evidence", 100)
		if err == nil {
			for _, record := range records {
				appendBundle(record.Content)
			}
		}
	}
	ordered := make([]EvidenceBundle, 0, len(bundles))
	keys := make([]string, 0, len(bundles))
	for key := range bundles {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ordered = append(ordered, bundles[key])
	}
	return ordered
}

// -- critic ------------------------------------------------------------------

type criticGap struct {
	GapID            string   `json:"gapId"`
	Question         string   `json:"question"`
	Severity         string   `json:"severity"`
	SuggestedQueries []string `json:"suggestedQueries"`
}

type criticDecision struct {
	Status      string      `json:"status"`
	Score       float64     `json:"score"`
	Gaps        []criticGap `json:"gaps"`
	Passthrough bool        `json:"passthrough,omitempty"`
}

type criticWireGap struct {
	GapID            string   `json:"gapId"`
	Question         string   `json:"question"`
	Description      string   `json:"description"`
	Gap              string   `json:"gap"`
	Severity         string   `json:"severity"`
	SuggestedQueries []string `json:"suggestedQueries"`
	Queries          []string `json:"queries"`
}

func decodeCriticOutput(content string) (criticDecision, error) {
	var wire struct {
		Status  string          `json:"status"`
		Verdict string          `json:"verdict"`
		Score   float64         `json:"score"`
		Gaps    []criticWireGap `json:"gaps"`
	}
	if err := decodeLoose(content, &wire); err != nil {
		return criticDecision{}, err
	}
	status := strings.ToUpper(strings.TrimSpace(wire.Status))
	if status == "" {
		status = strings.ToUpper(strings.TrimSpace(wire.Verdict))
	}
	decision := criticDecision{Status: status, Score: wire.Score, Gaps: make([]criticGap, 0, min(len(wire.Gaps), 4))}
	for _, candidate := range wire.Gaps {
		question := strings.TrimSpace(candidate.Question)
		if question == "" {
			question = strings.TrimSpace(candidate.Description)
		}
		if question == "" {
			question = strings.TrimSpace(candidate.Gap)
		}
		if question == "" {
			continue
		}
		queries := candidate.SuggestedQueries
		if len(queries) == 0 {
			queries = candidate.Queries
		}
		cleanQueries := make([]string, 0, min(len(queries), 3))
		for _, query := range queries {
			if query = strings.TrimSpace(query); query != "" {
				cleanQueries = append(cleanQueries, query)
				if len(cleanQueries) == 3 {
					break
				}
			}
		}
		if len(cleanQueries) == 0 {
			cleanQueries = []string{question}
		}
		decision.Gaps = append(decision.Gaps, criticGap{
			GapID: candidate.GapID, Question: question,
			Severity: strings.TrimSpace(candidate.Severity), SuggestedQueries: cleanQueries,
		})
		if len(decision.Gaps) == 4 {
			break
		}
	}
	if len(wire.Gaps) > 0 && len(decision.Gaps) == 0 {
		return criticDecision{}, fmt.Errorf("critic produced gaps without usable questions")
	}
	if decision.Status == "" {
		if decision.Score >= 0.75 && len(decision.Gaps) == 0 {
			decision.Status = "PASS"
		} else {
			decision.Status = "NEEDS_MORE_RESEARCH"
		}
	}
	switch decision.Status {
	case "PASS", "NEEDS_MORE_RESEARCH", "INSUFFICIENT_EVIDENCE":
	default:
		return criticDecision{}, fmt.Errorf("unsupported status %q", decision.Status)
	}
	return decision, nil
}

func runCritic(ctx context.Context, deps Deps, executionID string, envelope Envelope) (json.RawMessage, error) {
	decisionKey := fmt.Sprintf("critic-r%d", envelope.Round)
	// Carried PASS: the previous round already accepted the analysis.
	if upstreamVerdict(envelope.Goal) == "PASS" && envelope.Round > 1 {
		decision := criticDecision{Status: "PASS", Score: -1, Gaps: []criticGap{}, Passthrough: true}
		decision.Score = 0.99
		if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("gaps"), decisionKey, "application/json", decision); err != nil {
			return nil, err
		}
		output, _ := json.Marshal(decision)
		return output, nil
	}
	analysisRaw := ""
	for _, raw := range ExtractUpstreamOutputs(envelope.Goal) {
		if strings.Contains(raw, `"findings"`) {
			analysisRaw = raw
			break
		}
	}
	if analysisRaw == "" {
		records, _ := SearchMemory(ctx, deps.MCP, executionID, deps.Workdir("analysis"), fmt.Sprintf("analysis-r%d", envelope.Round), 5)
		for _, record := range records {
			if strings.Contains(record.Content, `"findings"`) {
				analysisRaw = record.Content
				break
			}
		}
	}
	if analysisRaw == "" {
		return nil, fmt.Errorf("critic round %d found no analysis to judge", envelope.Round)
	}
	system := "You are a research critic. Judge whether the analysis answers the original goal rigorously. " +
		"If specific, answerable sub-questions remain unaddressed, return NEEDS_MORE_RESEARCH with at most 4 gaps, each with 1-3 suggested search queries. " +
		"A score below 0.75 means more research is needed. When Round >= 3 do not force PASS: if the evidence base is still too thin, return INSUFFICIENT_EVIDENCE. " + jsonOnlyInstruction
	user := fmt.Sprintf("Original goal summary:\n%s\n\nRound: %d of 3\n\nAnalysis under review:\n%s\n\nReturn schema:\n%s",
		truncate(strings.TrimPrefix(envelope.Goal, envelopePrefix), 2000), envelope.Round, truncate(analysisRaw, 16000),
		`{"status":"PASS|NEEDS_MORE_RESEARCH|INSUFFICIENT_EVIDENCE","score":0.87,"gaps":[{"gapId":"gap-001","question":"...","severity":"high|medium|low","suggestedQueries":["..."]}]}`)
	content, err := chat(ctx, deps, executionID, system, user)
	if err != nil {
		return nil, err
	}
	decision, err := decodeCriticOutput(content)
	if err != nil {
		return nil, fmt.Errorf("critic output: %w", err)
	}
	if decision.Score >= 0 && decision.Score < 0.75 && decision.Status == "PASS" && envelope.Round < 3 {
		decision.Status = "NEEDS_MORE_RESEARCH"
	}
	// Terminal rule (roadmap §3.5): round three never forces convergence.
	// A still-unanswered analysis ends as INSUFFICIENT_EVIDENCE so the
	// downstream writer reports the shortfall honestly instead of faking
	// acceptance.
	applyCriticTerminalRule(&decision.Status, envelope.Round)
	if decision.Gaps == nil {
		decision.Gaps = []criticGap{}
	}
	if decision.Status == "NEEDS_MORE_RESEARCH" && len(decision.Gaps) == 0 {
		decision.Status = "PASS" // a demand for more research without gaps is vacuous
	}
	for index := range decision.Gaps {
		decision.Gaps[index].GapID = fmt.Sprintf("gap-r%d-%03d", envelope.Round, index+1)
		if len(decision.Gaps[index].SuggestedQueries) == 0 {
			decision.Gaps[index].SuggestedQueries = []string{decision.Gaps[index].Question}
		}
	}
	if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("gaps"), decisionKey, "application/json", decision); err != nil {
		return nil, err
	}
	if decision.Status == "NEEDS_MORE_RESEARCH" {
		for index, gap := range decision.Gaps {
			if index >= 4 {
				break
			}
			childGoal := encodeEnvelope(Envelope{
				Role: "search", Workflow: envelope.Workflow, Round: envelope.Round,
				Question: &Question{ID: gap.GapID, Question: gap.Question, Priority: 1, SearchQueries: gap.SuggestedQueries},
			})
			name := "search-gap-r" + fmt.Sprintf("%d-g%d", envelope.Round, index+1)
			if err := SpawnChild(ctx, deps.MCP, executionID, name, "research-search@1.0.0", childGoal, 3); err != nil {
				return nil, err
			}
		}
	}
	output, _ := json.Marshal(decision)
	return output, nil
}

// -- writer / validator ------------------------------------------------------

type section struct {
	Heading string `json:"heading"`
	Body    string `json:"body"`
}

type citation struct {
	Marker     string `json:"marker"`
	EvidenceID string `json:"evidenceId"`
	SourceID   string `json:"sourceId"`
	Quote      string `json:"quote"`
}

type reportDoc struct {
	Title     string     `json:"title"`
	Summary   string     `json:"summary"`
	Sections  []section  `json:"sections"`
	Citations []citation `json:"citations"`
	// InsufficientEvidence records an honest INSUFFICIENT_EVIDENCE verdict
	// from the final critic: the report ships, but declares the shortfall.
	InsufficientEvidence bool `json:"insufficientEvidence,omitempty"`
}

type reportWireSection struct {
	Heading string `json:"heading"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Content string `json:"content"`
	Text    string `json:"text"`
}

type reportWireCitation struct {
	Marker     string `json:"marker"`
	Citation   string `json:"citation"`
	EvidenceID string `json:"evidenceId"`
	ClaimID    string `json:"claimId"`
	SourceID   string `json:"sourceId"`
	Quote      string `json:"quote"`
	Evidence   string `json:"evidence"`
}

type reportWire struct {
	Title            string               `json:"title"`
	ReportTitle      string               `json:"reportTitle"`
	Summary          string               `json:"summary"`
	ExecutiveSummary string               `json:"executiveSummary"`
	Abstract         string               `json:"abstract"`
	Sections         []reportWireSection  `json:"sections"`
	Citations        []reportWireCitation `json:"citations"`
	Report           *reportWire          `json:"report"`
}

func decodeReportOutput(content string) (reportDoc, error) {
	var wire reportWire
	if err := decodeLoose(content, &wire); err != nil {
		return reportDoc{}, err
	}
	if wire.Report != nil && strings.TrimSpace(wire.Title) == "" && len(wire.Sections) == 0 {
		wire = *wire.Report
	}
	title := strings.TrimSpace(wire.Title)
	if title == "" {
		title = strings.TrimSpace(wire.ReportTitle)
	}
	summary := strings.TrimSpace(wire.Summary)
	if summary == "" {
		summary = strings.TrimSpace(wire.ExecutiveSummary)
	}
	if summary == "" {
		summary = strings.TrimSpace(wire.Abstract)
	}
	report := reportDoc{Title: title, Summary: summary, Sections: make([]section, 0, len(wire.Sections)), Citations: make([]citation, 0, len(wire.Citations))}
	for _, candidate := range wire.Sections {
		heading := strings.TrimSpace(candidate.Heading)
		if heading == "" {
			heading = strings.TrimSpace(candidate.Title)
		}
		body := strings.TrimSpace(candidate.Body)
		if body == "" {
			body = strings.TrimSpace(candidate.Content)
		}
		if body == "" {
			body = strings.TrimSpace(candidate.Text)
		}
		if heading != "" && body != "" {
			report.Sections = append(report.Sections, section{Heading: heading, Body: body})
		}
	}
	for index, candidate := range wire.Citations {
		marker := strings.TrimSpace(candidate.Marker)
		if marker == "" {
			marker = strings.TrimSpace(candidate.Citation)
		}
		if marker == "" {
			marker = fmt.Sprintf("[%d]", index+1)
		}
		evidenceID := strings.TrimSpace(candidate.EvidenceID)
		if evidenceID == "" {
			evidenceID = strings.TrimSpace(candidate.ClaimID)
		}
		quote := strings.TrimSpace(candidate.Quote)
		if quote == "" {
			quote = strings.TrimSpace(candidate.Evidence)
		}
		sourceID := strings.TrimSpace(candidate.SourceID)
		if sourceID == "" {
			if prefix, _, ok := strings.Cut(evidenceID, "-claim-"); ok {
				sourceID = prefix
			}
		}
		report.Citations = append(report.Citations, citation{
			Marker: marker, EvidenceID: evidenceID, SourceID: sourceID, Quote: quote,
		})
	}
	if report.Title == "" || report.Summary == "" || len(report.Sections) == 0 {
		return reportDoc{}, fmt.Errorf("writer produced an incomplete report")
	}
	return report, nil
}

func runWriter(ctx context.Context, deps Deps, executionID string, envelope Envelope, feedback string) (json.RawMessage, string, error) {
	bundles := collectEvidence(ctx, deps, executionID, envelope)
	if len(bundles) == 0 {
		return nil, "", fmt.Errorf("writer found no evidence")
	}
	analysisRaw := ""
	for _, raw := range ExtractUpstreamOutputs(envelope.Goal) {
		if strings.Contains(raw, `"findings"`) {
			analysisRaw = raw
			break
		}
	}
	system := "You write the final research report strictly from the supplied findings and evidence. " +
		"Every non-obvious statement cites extracted evidence using markers like [1]; each citation quotes the supporting passage VERBATIM from that claim's evidence text. " +
		"Use only the exact field names in the requested schema. " + jsonOnlyInstruction
	user := fmt.Sprintf("Findings:\n%s\n\nEvidence bundles:\n%s\n\nReturn schema:\n%s",
		truncate(analysisRaw, 12000), truncate(renderBundles(bundles), 20000),
		`{"title":"...","summary":"...","sections":[{"heading":"...","body":"... [1]"}],"citations":[{"marker":"[1]","evidenceId":"src-001-claim-001","sourceId":"src-001","quote":"exact verbatim passage"}]}`)
	if feedback != "" {
		user += "\n\nRevision feedback from the citation validator:\n" + feedback
	}
	content, err := chat(ctx, deps, executionID, system, user)
	if err != nil {
		return nil, "", err
	}
	report, err := decodeReportOutput(content)
	if err != nil {
		return nil, "", fmt.Errorf("writer output: %w", err)
	}
	report.InsufficientEvidence = upstreamVerdict(envelope.Goal) == "INSUFFICIENT_EVIDENCE"
	encoded, _ := json.Marshal(report)
	if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("report"), "report-draft", "application/json", report); err != nil {
		return nil, "", err
	}
	return encoded, "report-draft", nil
}

type validationVerdict struct {
	Valid             bool    `json:"valid"`
	CitationCoverage  float64 `json:"citationCoverage"`
	UnsupportedClaims int     `json:"unsupportedClaims"`
	Retries           int     `json:"retries"`
	// InsufficientEvidence mirrors the final critic's honest verdict when
	// the evidence base could not answer the goal within the round budget.
	InsufficientEvidence bool `json:"insufficientEvidence,omitempty"`
}

func runValidator(ctx context.Context, deps Deps, executionID string, envelope Envelope) (json.RawMessage, error) {
	const threshold = 0.90
	retries := 0
	for {
		raw, key, err := loadReportDraft(ctx, deps, executionID, envelope)
		if err != nil {
			return nil, err
		}
		bundles := collectEvidence(ctx, deps, executionID, envelope)
		verdict := gradeCitations(raw, bundles)
		// Mirror the shortfall from the draft itself (the writer stamps it
		// from the final critic verdict); the validator's own rendered goal
		// does not carry the critic block transitively.
		var probe struct {
			InsufficientEvidence bool `json:"insufficientEvidence"`
		}
		_ = json.Unmarshal(raw, &probe)
		verdict.InsufficientEvidence =
			upstreamVerdict(envelope.Goal) == "INSUFFICIENT_EVIDENCE" || probe.InsufficientEvidence
		if verdict.CitationCoverage >= threshold && verdict.UnsupportedClaims == 0 {
			verdict.Retries = retries
			if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("report"), "report", "application/json", raw); err != nil {
				return nil, err
			}
			if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("report"), "validation", "application/json", verdict); err != nil {
				return nil, err
			}
			_ = key
			output, _ := json.Marshal(verdict)
			return output, nil
		}
		if retries >= 2 {
			verdict.Retries = retries
			// Ship the best effort with an honest verdict; the workflow
			// completes but reports the shortfall.
			if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("report"), "report", "application/json", raw); err != nil {
				return nil, err
			}
			if err := PutMemory(ctx, deps.MCP, executionID, deps.Workdir("report"), "validation", "application/json", verdict); err != nil {
				return nil, err
			}
			output, _ := json.Marshal(verdict)
			return output, nil
		}
		retries++
		// The retry counter keeps every revision prompt byte-distinct so the
		// brokered idempotency key (attempt + argument digest) never replays
		// a completed model call.
		feedback := fmt.Sprintf(
			"Revision %d: citation coverage %.2f is below %.2f with %d unsupported citations. Re-quote passages verbatim from the listed evidence and cover every finding.",
			retries, verdict.CitationCoverage, threshold, verdict.UnsupportedClaims)
		if _, _, err := runWriter(ctx, deps, executionID, envelope, feedback); err != nil {
			return nil, fmt.Errorf("writer revision failed: %w", err)
		}
	}
}

func loadReportDraft(ctx context.Context, deps Deps, executionID string, envelope Envelope) (json.RawMessage, string, error) {
	// Memory wins over the rendered upstream block: the upstream copy is
	// the writer's ORIGINAL draft, while revisions land here between
	// validation passes. Falling back to upstream keeps cold-start validators
	// working when the draft was not persisted.
	// The hybrid memory index only matches tokens present in record content,
	// so query the JSON field name shared by every draft instead of the key.
	records, err := SearchMemory(ctx, deps.MCP, executionID, deps.Workdir("report"), "citations", 10)
	if err != nil {
		return nil, "", err
	}
	for _, record := range records {
		if record.Key == "report-draft" && strings.Contains(record.Content, `"citations"`) {
			return json.RawMessage(record.Content), "report-draft", nil
		}
	}
	for _, raw := range ExtractUpstreamOutputs(envelope.Goal) {
		if strings.Contains(raw, `"citations"`) {
			return json.RawMessage(firstJSONObject(raw)), "upstream", nil
		}
	}
	return nil, "", fmt.Errorf("no report draft found for validation")
}

// gradeCitations scores a report against the evidence base under STRICT
// grounding rules (roadmap §3.3): a citation counts as supported only when
// its evidenceId names an existing claim AND its quote is a non-empty,
// whitespace-normalized substring of THAT claim's evidence. Empty quotes,
// unknown claim ids, and passages borrowed from different evidence are all
// rejected — there is no corpus-wide fallback.
func gradeCitations(raw json.RawMessage, bundles []EvidenceBundle) validationVerdict {
	var report reportDoc
	if err := json.Unmarshal(raw, &report); err != nil {
		return validationVerdict{}
	}
	evidenceText := map[string]string{}
	for _, bundle := range bundles {
		for _, claim := range bundle.Claims {
			evidenceText[claim.ClaimID] = normalizeSpace(claim.Evidence)
		}
	}
	supported := 0
	unsupported := 0
	for _, cite := range report.Citations {
		text, known := evidenceText[cite.EvidenceID]
		switch {
		case cite.Quote == "", !known:
			unsupported++
			continue
		case strings.Contains(text, normalizeSpace(cite.Quote)):
			supported++
		default:
			unsupported++
		}
	}
	total := len(report.Citations)
	coverage := 0.0
	if total > 0 {
		coverage = float64(supported) / float64(total)
	}
	return validationVerdict{Valid: total > 0 && coverage >= 0.90 && unsupported == 0,
		CitationCoverage: coverage, UnsupportedClaims: unsupported}
}

// -- helpers -----------------------------------------------------------------

func encodeEnvelope(envelope Envelope) string {
	encoded, _ := json.Marshal(envelope)
	return envelopePrefix + string(encoded)
}

func decodeLoose(content string, target any) error {
	document := firstJSONObject(content)
	if document == "" {
		return fmt.Errorf("no JSON object found in %.120q", content)
	}
	if err := json.Unmarshal([]byte(document), target); err != nil {
		return err
	}
	return nil
}

// firstJSONObject extracts the first balanced top-level JSON object.
func firstJSONObject(text string) string {
	start := strings.Index(text, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(text); index++ {
		char := text[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : index+1]
			}
		}
	}
	return ""
}

func normalizeSpace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

func renderBundles(bundles []EvidenceBundle) string {
	parts := make([]string, 0, len(bundles))
	for _, bundle := range bundles {
		encoded, err := json.Marshal(bundle)
		if err != nil {
			continue
		}
		parts = append(parts, string(encoded))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// applyCriticTerminalRule enforces the third terminal state: from round
// three on, a critic that still cannot accept the analysis must declare
// INSUFFICIENT_EVIDENCE rather than being forced into PASS.
func applyCriticTerminalRule(status *string, round int) {
	if round >= 3 && *status != "PASS" && *status != "" {
		*status = "INSUFFICIENT_EVIDENCE"
	}
}
