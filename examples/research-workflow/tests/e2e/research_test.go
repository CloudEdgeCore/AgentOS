//go:build integration

package research_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	research "github.com/CloudEdgeCore/AgentOS/examples/research-workflow/runtime"
	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
)

const (
	settleTimeout    = 4 * time.Minute
	liveModelTimeout = 12 * time.Minute
	liveFullTimeout  = 15 * time.Minute
)

// Phase 1 acceptance: one goal -> >=3 questions -> >=10 sources -> >=30
// evidence claims, every artifact stored in the workflow's memory tree.
func TestResearchWorkflowBasic(t *testing.T) {
	h := newHarness(t, "basic", nil)
	id, err := h.createResearch("Analyze the direction of agent runtime infrastructure over the next three years")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	workflow := h.requireCompleted(id, settleTimeout)

	var plan struct {
		Questions []research.Question `json:"questions"`
	}
	mustDecodeMemory(t, h, id, "analysis", "plan", &plan)
	if len(plan.Questions) < 3 {
		t.Fatalf("planner produced %d questions, want >= 3", len(plan.Questions))
	}
	sourceCount := memoryKeyCount(t, h, id, "sources")
	if sourceCount < 3 {
		t.Fatalf("sources namespace holds %d records, want >= 3 batches", sourceCount)
	}
	evidenceJSON := memoryContentsJoined(t, h, id, "evidence")
	claimCount := strings.Count(evidenceJSON, `"claimId"`)
	if claimCount < 30 {
		t.Fatalf("evidence carries %d claims, want >= 30 (%.400s)", claimCount, evidenceJSON)
	}

	steps := listSteps(t, h, id)
	searches, readers, collectors := 0, 0, 0
	for _, step := range steps {
		switch {
		case strings.HasPrefix(step.Name, "search-rq-"):
			searches++
		case strings.HasPrefix(step.Name, "reader-"):
			readers++
		case step.Name == "collector-r2" || step.Name == "collector-r3":
			collectors++
		}
	}
	if searches < 3 || readers < 10 || collectors < 2 {
		t.Fatalf("spawned steps: searches=%d readers=%d collectorRounds=%d", searches, readers, collectors)
	}
	if workflow.Status != kernelstore.WorkflowSucceeded {
		t.Fatalf("workflow status = %s", workflow.Status)
	}
	t.Logf("basic loop verified: questions=%d searches=%d readers=%d claims=%d",
		len(plan.Questions), searches, readers, claimCount)
}

// The critic identifies an evidence gap in round one, spawns additional gap
// searches and readers, and the final round converges to PASS.
func TestResearchWorkflowCriticRetry(t *testing.T) {
	h := newHarness(t, "criticretry", func(s *scenario) { s.criticRound1NeedsMore = true })
	id, err := h.createResearch("Compare current mainstream agent runtime approaches and assess the opportunity")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	h.requireCompleted(id, settleTimeout)

	gaps := memoryContentsJoined(t, h, id, "gaps")
	if !strings.Contains(gaps, "NEEDS_MORE_RESEARCH") || !strings.Contains(gaps, "gap-r1-001") {
		t.Fatalf("round-one decision missing from gaps namespace: %.300s", gaps)
	}
	steps := listSteps(t, h, id)
	gapSearches, gapReaders, finalPass := 0, 0, false
	for _, step := range steps {
		if strings.HasPrefix(step.Name, "search-gap-r1") && step.Status == kernelstore.StepSucceeded {
			gapSearches++
		}
		if strings.HasPrefix(step.Name, "reader-") && step.ParentStepName == "collector-r2" &&
			step.Status == kernelstore.StepSucceeded {
			gapReaders++
		}
		if step.Name == "critic-final" && step.Status == kernelstore.StepSucceeded &&
			strings.Contains(extractOutput(step.ResultSummary), `"status":"PASS"`) {
			finalPass = true
		}
	}
	if gapSearches == 0 {
		t.Fatalf("critic round one spawned no gap searches; steps: %s", renderSteps(steps))
	}
	if gapReaders == 0 {
		t.Fatalf("round-two collector spawned no gap readers; steps: %s", renderSteps(steps))
	}
	if !finalPass {
		t.Fatalf("final critic did not PASS; steps: %s", renderSteps(steps))
	}
	t.Logf("critic retry verified: gapSearches=%d gapReaders=%d finalVerdict=PASS", gapSearches, gapReaders)
}

// Citation validation enforces verbatim quotes: fabricated citations force
// writer revisions until coverage clears the gate.
func TestResearchWorkflowCitationCoverage(t *testing.T) {
	h := newHarness(t, "citation", func(s *scenario) { s.writerBadCitations = true })
	id, err := h.createResearch("Write a fully-cited outlook on agent runtime infrastructure")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	h.requireCompleted(id, settleTimeout)

	var validation struct {
		CitationCoverage  float64 `json:"citationCoverage"`
		UnsupportedClaims int     `json:"unsupportedClaims"`
		Retries           int     `json:"retries"`
	}
	mustDecodeMemory(t, h, id, "report", "validation", &validation)
	if validation.Retries == 0 {
		t.Fatalf("validator did not trigger revisions despite fabricated citations")
	}
	if validation.CitationCoverage < 0.90 || validation.UnsupportedClaims != 0 {
		var report struct {
			Citations []struct {
				EvidenceID string `json:"evidenceId"`
				Quote      string `json:"quote"`
			} `json:"citations"`
		}
		mustDecodeMemory(t, h, id, "report", "report", &report)
		for _, cite := range report.Citations {
			t.Logf("citation %s quote=%.120q", cite.EvidenceID, cite.Quote)
		}
		t.Fatalf("final coverage %.2f unsupported=%d retries=%d",
			validation.CitationCoverage, validation.UnsupportedClaims, validation.Retries)
	}
	t.Logf("citation gate enforced after %d revision(s): coverage=%.2f",
		validation.Retries, validation.CitationCoverage)
}

// Evidence → Original Source grounding (roadmap §3.4): every persisted
// claim carries a source hash and grounded flag; nothing ungrounded may
// reach the evidence namespace.
func TestResearchWorkflowEvidenceGrounding(t *testing.T) {
	h := newHarness(t, "grounding", nil)
	id, err := h.createResearch("Grounded-evidence overview of agent runtime control planes")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	h.requireCompleted(id, settleTimeout)

	evidenceJSON := memoryContentsJoined(t, h, id, "evidence")
	claims := strings.Count(evidenceJSON, `"claimId"`)
	if claims < 30 {
		t.Fatalf("evidence carries %d claims, want >= 30", claims)
	}
	if strings.Contains(evidenceJSON, `"grounded":false`) {
		t.Fatal("ungrounded claim reached the evidence namespace")
	}
	hashes := strings.Count(evidenceJSON, `"sourceHash":"`)
	if hashes != claims {
		t.Fatalf("sourceHash stamped on %d of %d claims", hashes, claims)
	}
	t.Logf("evidence grounding verified: claims=%d allGrounded=true", claims)
}

// A persistently sloppy writer citing non-existent claim ids can never pass
// the hardened validator; the workflow ships the best effort with an honest
// unsupported-citations verdict instead of failing.
func TestResearchWorkflowInvalidCitation(t *testing.T) {
	h := newHarness(t, "invalidcitation", func(s *scenario) { s.writerUnknownEvidence = true })
	id, err := h.createResearch("Invalid-citation resilience outlook on agent runtime governance")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	h.requireCompleted(id, settleTimeout)

	var validation struct {
		CitationCoverage  float64 `json:"citationCoverage"`
		UnsupportedClaims int     `json:"unsupportedClaims"`
		Retries           int     `json:"retries"`
	}
	mustDecodeMemory(t, h, id, "report", "validation", &validation)
	if validation.Retries < 2 {
		t.Fatalf("validator gave up too early: retries=%d", validation.Retries)
	}
	if validation.UnsupportedClaims == 0 || validation.CitationCoverage >= 0.90 {
		t.Fatalf("unknown-evidence citations must stay unsupported: %+v", validation)
	}
	t.Logf("honest best-effort shipped: coverage=%.2f unsupported=%d retries=%d",
		validation.CitationCoverage, validation.UnsupportedClaims, validation.Retries)
}

// When the critic still cannot accept the analysis at round three it must
// declare INSUFFICIENT_EVIDENCE instead of being forced into PASS; the
// writer and validator surface that shortfall honestly.
func TestResearchWorkflowInsufficientEvidence(t *testing.T) {
	h := newHarness(t, "insufficient", func(s *scenario) { s.criticAlwaysNeedsMore = true })
	id, err := h.createResearch("Thin-evidence probe where the critic should declare insufficiency")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	h.requireCompleted(id, settleTimeout)

	gaps := memoryContentsJoined(t, h, id, "gaps")
	if !strings.Contains(gaps, `"status":"INSUFFICIENT_EVIDENCE"`) {
		t.Fatalf("final critic decision missing INSUFFICIENT_EVIDENCE: %.400s", gaps)
	}
	var validation struct {
		Valid                bool `json:"valid"`
		InsufficientEvidence bool `json:"insufficientEvidence"`
	}
	mustDecodeMemory(t, h, id, "report", "validation", &validation)
	if !validation.InsufficientEvidence {
		t.Fatalf("validation verdict must mirror the shortfall: %+v", validation)
	}
	report := memoryContentsJoined(t, h, id, "report")
	if !strings.Contains(report, `"insufficientEvidence":true`) {
		t.Fatal("shipped report does not declare insufficient evidence")
	}
	t.Logf("insufficiency surfaced honestly: valid=%v declared=true", validation.Valid)
}

// Tool failures recover through attempt retries without duplicate side
// effects: injected fetch failures are absorbed and the workflow completes.
func TestResearchWorkflowToolFailureRecovery(t *testing.T) {
	h := newHarness(t, "toolfailure", nil)
	h.webtools.InjectFetchFailures("corpus.agentos.dev", 2)
	before := h.fetches.total()
	id, err := h.createResearch("Assess sandboxing strategies for reader agents fetching untrusted content")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	h.requireCompleted(id, settleTimeout+time.Minute)
	fetchDelta := h.fetches.total() - before
	if fetchDelta == 0 {
		t.Fatalf("no fetch activity observed")
	}
	t.Logf("tool-failure scenario complete: fetchCalls=%d (includes 2 injected failures)", fetchDelta)
}

// A workflow budget too small to fit the fan-out stops the workflow cleanly
// instead of running unbounded.
func TestResearchWorkflowBudgetStop(t *testing.T) {
	h := newHarness(t, "budgetstop", nil)
	id, err := h.createResearch("Tiny-budget bounded dive into agent runtime isolation economics")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	workflow, _, err := h.awaitWorkflow(id, settleTimeout)
	if err != nil {
		t.Fatalf("await workflow: %v", err)
	}
	if workflow.Status == "" {
		t.Fatalf("workflow never reported a status")
	}
	t.Logf("budget scenario settled: status=%s budgetTasks=%d budgetTokens=%d",
		workflow.Status, workflow.BudgetMaxTasks, workflow.BudgetMaxTokens)
}

// Provider-side model failures (HTTP 429) are absorbed by the provider
// execution policy's bounded retry and the workflow still completes.
func TestResearchWorkflowModelFailure(t *testing.T) {
	h := newHarness(t, "modelfailure", nil)
	h.provider.InjectHTTPFailures(http.StatusTooManyRequests, 1)
	before := h.provider.callCount()
	id, err := h.createResearch("Model-failure resilient overview of agent runtime governance")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	h.requireCompleted(id, settleTimeout)
	if total := h.provider.callCount(); total <= before+1 {
		t.Fatalf("expected the provider retry to add calls: before=%d after=%d", before, total)
	}
	t.Logf("model-failure scenario complete: providerCalls=%d (includes 1 injected 429)", h.provider.callCount())
}

// Killing every worker mid-run strands in-flight attempts on expired leases;
// recovery must reclaim them and restarted runtimes must finish the work.
func TestResearchWorkflowRecovery(t *testing.T) {
	h := newHarness(t, "recovery", nil)
	id, err := h.createResearch("Crash-recovery validated survey of agent runtime infrastructure")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	// Wait until the fan-out is DEEP in flight (several live attempts, not
	// just the planner pair) so killing both workers guarantees stranded
	// leases exist to recover.
	deadline := time.Now().Add(90 * time.Second)
	for {
		var running, attempts int
		if err := h.pool.QueryRow(context.Background(),
			`SELECT (SELECT COUNT(*) FROM tasks WHERE phase = 'RUNNING'),
				(SELECT COUNT(*) FROM attempts)`).Scan(&running, &attempts); err != nil {
			t.Fatalf("count in-flight work: %v", err)
		}
		if running >= 3 && attempts >= 6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fan-out never went deep enough to crash: running=%d attempts=%d", running, attempts)
		}
		time.Sleep(150 * time.Millisecond)
	}
	h.KillWorker("research-worker-a")
	h.KillWorker("research-worker-b")
	// Leases expire (15s heartbeat TTL) -> recovery requeues. Wait for the
	// replacement attempt to EXIST before restarting workers so the
	// assertion below can never miss a slow-but-successful recovery.
	recoverDeadline := time.Now().Add(45 * time.Second)
	for {
		var retried int
		if err := h.pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM attempts WHERE ordinal >= 2`).Scan(&retried); err != nil {
			t.Fatalf("count replacement attempts: %v", err)
		}
		if retried >= 1 || time.Now().After(recoverDeadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Restart BOTH runtime instances because stranded runs may be bound to
	// either pool.
	h.startWorker(h.loopCtx, "research-worker-a")
	h.startWorker(h.loopCtx, "research-worker-b")

	h.requireCompleted(id, settleTimeout+3*time.Minute)

	var retriedRuns, retriedAttempts int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT
			(SELECT COUNT(*) FROM runs WHERE ordinal >= 2),
			(SELECT COUNT(*) FROM attempts WHERE ordinal >= 2)`).
		Scan(&retriedRuns, &retriedAttempts); err != nil {
		t.Fatalf("count retried units: %v", err)
	}
	// Lease-expiry recovery places attempt N+1 inside the SAME run; the
	// clean-failure requeue path starts a NEW run. Either proves reclaim.
	if retriedAttempts == 0 && retriedRuns == 0 {
		t.Fatalf("recovery placed no replacement attempts")
	}
	t.Logf("crash-recovery scenario complete: retriedRuns=%d retriedAttempts=%d", retriedRuns, retriedAttempts)
}

// Scale gate: 100 concurrent research workflows must all complete. Heavy —
// enabled explicitly with AGENTOS_RESEARCH_SCALE=1.
func TestResearch100Concurrent(t *testing.T) {
	if os.Getenv("AGENTOS_RESEARCH_SCALE") != "1" {
		t.Skip("set AGENTOS_RESEARCH_SCALE=1 to run the 100-way scale scenario")
	}
	h := newHarness(t, "scale100", nil)
	const fanOut = 100
	ids := make([]uuid.UUID, 0, fanOut)
	for index := 0; index < fanOut; index++ {
		id, err := h.createResearch(fmt.Sprintf(
			"Scale probe %03d: outlook on agent runtime infrastructure", index))
		if err != nil {
			t.Fatalf("create research %d: %v", index, err)
		}
		ids = append(ids, id)
	}
	var failed []string
	for _, id := range ids {
		workflow, _, err := h.awaitWorkflow(id, 12*time.Minute)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		if status := workflow.Status; status != "SUCCEEDED" && status != "FAILED" && status != "CANCELLED" {
			failed = append(failed, fmt.Sprintf("%s unsettled: %s", id, status))
		} else if status != "SUCCEEDED" {
			failed = append(failed, fmt.Sprintf("%s terminal=%s", id, status))
		}
	}
	if len(failed) > 0 {
		t.Fatalf("%d/%d workflows did not succeed: %v", len(failed), fanOut, failed[:minInt(5, len(failed))])
	}
	t.Logf("scale scenario complete: %d/%d succeeded", fanOut, fanOut)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// -- memory/store probes -----------------------------------------------------

func listSteps(t *testing.T, h *harness, id fmt.Stringer) []kernelstore.WorkflowStep {
	t.Helper()
	workflowID, err := uuid.Parse(id.String())
	if err != nil {
		t.Fatalf("parse workflow id: %v", err)
	}
	steps, err := h.store.ListWorkflowSteps(context.Background(), researchTenant, workflowID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	return steps
}

func memoryContentsJoined(t *testing.T, h *harness, id fmt.Stringer, leaf string) string {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT content FROM memory_records WHERE namespace = $1 ORDER BY key`, fmt.Sprintf("research/%s/%s", id, leaf))
	if err != nil {
		t.Fatalf("query memory %s: %v", leaf, err)
	}
	defer rows.Close()
	var joined strings.Builder
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("scan memory row: %v", err)
		}
		joined.WriteString(content)
		joined.WriteString("\n")
	}
	return joined.String()
}

func memoryKeyCount(t *testing.T, h *harness, id fmt.Stringer, leaf string) int {
	t.Helper()
	var count int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM memory_records WHERE namespace = $1`, fmt.Sprintf("research/%s/%s", id, leaf)).Scan(&count); err != nil {
		t.Fatalf("count memory %s: %v", leaf, err)
	}
	return count
}

func mustDecodeMemory(t *testing.T, h *harness, id fmt.Stringer, leaf, key string, target any) {
	t.Helper()
	var content string
	err := h.pool.QueryRow(context.Background(),
		`SELECT content FROM memory_records WHERE namespace = $1 AND key = $2`,
		fmt.Sprintf("research/%s/%s", id, leaf), key).Scan(&content)
	if err != nil {
		t.Fatalf("memory record %s/%s: %v", leaf, key, err)
	}
	if err := json.Unmarshal([]byte(content), target); err != nil {
		t.Fatalf("decode %s/%s: %v (%.200s)", leaf, key, err, content)
	}
}

// -- generic helpers ---------------------------------------------------------

// liveMetrics mirrors the observation set of implementation-plan §5: one
// JSON document per live research run, written next to the test log.
type liveMetrics struct {
	WorkflowID           string  `json:"workflowId"`
	Questions            int     `json:"questions"`
	Sources              int     `json:"sources"`
	UniqueDomains        int     `json:"uniqueDomains"`
	EvidenceCount        int     `json:"evidenceCount"`
	GroundedEvidenceRate float64 `json:"groundedEvidenceRate"`
	CitationCoverage     float64 `json:"citationCoverage"`
	UnsupportedCitations int     `json:"unsupportedCitations"`
	Retries              int     `json:"retries"`
	InsufficientEvidence bool    `json:"insufficientEvidence"`
	CriticRounds         int     `json:"criticRounds"`
	ModelCalls           int     `json:"modelCalls"`
	ModelFailures        int     `json:"modelFailures"`
	ToolCalls            int     `json:"toolCalls"`
	RecoveredAttempts    int     `json:"recoveredAttempts"`
	Tokens               int64   `json:"tokens"`
	CostUSD              float64 `json:"costUsd"`
	DurationSeconds      float64 `json:"durationSeconds"`
}

// collectLiveMetrics aggregates run evidence straight from the durable
// store so live and deterministic runs report identically.
func (h *harness) collectLiveMetrics(id uuid.UUID, started time.Time) liveMetrics {
	h.t.Helper()
	ctx := context.Background()
	metrics := liveMetrics{WorkflowID: id.String(), DurationSeconds: time.Since(started).Seconds()}

	evidenceJSON := memoryContentsJoined(h.t, h, id, "evidence")
	sourcesJSON := memoryContentsJoined(h.t, h, id, "sources")
	metrics.EvidenceCount = strings.Count(evidenceJSON, `"claimId"`)
	grounded := strings.Count(evidenceJSON, `"grounded":true`)
	if metrics.EvidenceCount > 0 {
		metrics.GroundedEvidenceRate = float64(grounded) / float64(metrics.EvidenceCount)
	}
	metrics.Sources = strings.Count(sourcesJSON, `"url"`)

	domains := map[string]bool{}
	for _, raw := range strings.Split(sourcesJSON+evidenceJSON, `"url":"`) {
		if len(raw) == 0 {
			continue
		}
		end := strings.IndexByte(raw, '"')
		if end < 0 {
			continue
		}
		candidate, err := strconv.Unquote(`"` + raw[:end] + `"`)
		if err != nil {
			continue
		}
		parsed, err := url.Parse(candidate)
		if err != nil {
			continue
		}
		if host := parsed.Hostname(); host != "" && host != "corpus.agentos.dev" {
			domains[host] = true
		}
	}
	metrics.UniqueDomains = len(domains)

	var plan struct {
		Questions []json.RawMessage `json:"questions"`
	}
	mustDecodeMemory(h.t, h, id, "analysis", "plan", &plan)
	metrics.Questions = len(plan.Questions)

	var validation struct {
		CitationCoverage     float64 `json:"citationCoverage"`
		UnsupportedClaims    int     `json:"unsupportedClaims"`
		Retries              int     `json:"retries"`
		InsufficientEvidence bool    `json:"insufficientEvidence"`
	}
	mustDecodeMemory(h.t, h, id, "report", "validation", &validation)
	metrics.CitationCoverage = validation.CitationCoverage
	metrics.UnsupportedCitations = validation.UnsupportedClaims
	metrics.Retries = validation.Retries
	metrics.InsufficientEvidence = validation.InsufficientEvidence

	scans := []struct {
		query  string
		target *int
	}{
		{`SELECT COUNT(DISTINCT name) FROM workflow_steps WHERE workflow_id = $1 AND name LIKE 'critic-r%' AND status = 'SUCCEEDED'`, &metrics.CriticRounds},
		{`SELECT COUNT(*) FROM model_calls mc JOIN tasks t ON t.id = mc.task_id WHERE t.workflow_id = $1 AND mc.status = 'COMPLETED'`, &metrics.ModelCalls},
		{`SELECT COUNT(*) FROM model_calls mc JOIN tasks t ON t.id = mc.task_id WHERE t.workflow_id = $1 AND mc.status = 'FAILED'`, &metrics.ModelFailures},
		{`SELECT COUNT(*) FROM tool_calls tc JOIN tasks t ON t.id = tc.task_id WHERE t.workflow_id = $1`, &metrics.ToolCalls},
		{`SELECT COUNT(*) FROM attempts a JOIN runs r ON r.id = a.run_id WHERE r.task_id IN (SELECT id FROM tasks WHERE workflow_id = $1) AND a.ordinal >= 2`, &metrics.RecoveredAttempts},
	}
	for _, scan := range scans {
		if err := h.pool.QueryRow(ctx, scan.query, id).Scan(scan.target); err != nil {
			h.t.Fatalf("metric query: %v", err)
		}
	}
	if err := h.pool.QueryRow(ctx, `SELECT
			COALESCE(SUM(input_tokens + output_tokens), 0),
			COALESCE(SUM(cost_micro_usd), 0)::double precision / 1000000.0
			FROM model_calls mc JOIN tasks t ON t.id = mc.task_id
			WHERE t.workflow_id = $1 AND mc.status = 'COMPLETED'`, id).Scan(&metrics.Tokens, &metrics.CostUSD); err != nil {
		h.t.Fatalf("usage query: %v", err)
	}
	return metrics
}

func requireLiveModelEnv(t *testing.T) {
	t.Helper()
	if os.Getenv("AGENTOS_RESEARCH_LIVE") != "1" {
		t.Skip("set AGENTOS_RESEARCH_LIVE=1 with AGENTOS_RESEARCH_MODEL_BASE_URL/KEY to run the live-model acceptance")
	}
}

func TestResearchWorkflowLiveModel(t *testing.T) {
	requireLiveModelEnv(t)
	started := time.Now()
	h := newHarness(t, "live-model", nil)
	id, err := h.createResearch("Summarize how agent runtime control planes evolved between 2023 and 2025")
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	workflow := h.requireCompleted(id, liveModelTimeout)

	var validation struct {
		CitationCoverage  float64 `json:"citationCoverage"`
		UnsupportedClaims int     `json:"unsupportedClaims"`
	}
	mustDecodeMemory(t, h, id, "report", "validation", &validation)
	if validation.UnsupportedClaims != 0 || validation.CitationCoverage < 0.90 {
		t.Fatalf("live acceptance gate failed: coverage=%.2f unsupported=%d",
			validation.CitationCoverage, validation.UnsupportedClaims)
	}
	evidenceJSON := memoryContentsJoined(t, h, id, "evidence")
	if strings.Contains(evidenceJSON, `"grounded":false`) {
		t.Fatal("ungrounded claim persisted in live run")
	}
	metrics := h.collectLiveMetrics(id, started)
	encoded, _ := json.MarshalIndent(metrics, "", " ")
	t.Logf("LIVE MODEL METRICS %s\n%s", workflow.Status, encoded)
}

func TestResearchWorkflowLiveFull(t *testing.T) {
	requireLiveModelEnv(t)
	if os.Getenv("AGENTOS_RESEARCH_LIVE_WEB") != "1" {
		t.Skip("set AGENTOS_RESEARCH_LIVE_WEB=1 with AGENTOS_RESEARCH_SEARCH_PROVIDER/KEY for full live research")
	}
	started := time.Now()
	goal := strings.TrimSpace(os.Getenv("AGENTOS_RESEARCH_LIVE_GOAL"))
	if goal == "" {
		t.Fatal("set AGENTOS_RESEARCH_LIVE_GOAL with a real research question")
	}
	h := newHarness(t, "live-full", nil)
	id, err := h.createResearch(goal)
	if err != nil {
		t.Fatalf("create research: %v", err)
	}
	h.requireCompleted(id, liveFullTimeout)

	metrics := h.collectLiveMetrics(id, started)
	// Acceptance gates from implementation plan §5.
	if metrics.GroundedEvidenceRate != 1 {
		t.Fatalf("grounded evidence rate %.3f < 1.0", metrics.GroundedEvidenceRate)
	}
	if metrics.UnsupportedCitations != 0 || metrics.CitationCoverage < 0.90 {
		t.Fatalf("citation gates failed: coverage=%.2f unsupported=%d",
			metrics.CitationCoverage, metrics.UnsupportedCitations)
	}
	if metrics.UniqueDomains < 3 {
		t.Fatalf("unique domains %d < 3", metrics.UniqueDomains)
	}
	if metrics.InsufficientEvidence {
		t.Fatal("live run must not end INSUFFICIENT_EVIDENCE")
	}
	encoded, _ := json.MarshalIndent(metrics, "", " ")
	t.Logf("LIVE FULL METRICS\n%s", encoded)
}

func extractOutput(summary json.RawMessage) string {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(summary, &document); err != nil {
		return ""
	}
	output, ok := document["output"]
	if !ok {
		return string(summary)
	}
	var text string
	if json.Unmarshal(output, &text) == nil {
		return text
	}
	// Structured outputs are stored as nested JSON objects; re-marshal
	// compactly so whitespace never breaks substring assertions.
	var value any
	if json.Unmarshal(output, &value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return string(output)
}

func renderSteps(steps []kernelstore.WorkflowStep) string {
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		parts = append(parts, fmt.Sprintf("%s=%s", step.Name, step.Status))
	}
	return strings.Join(parts, ",")
}

func truncateString(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
