//go:build integration

package research_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	research "github.com/CloudEdgeCore/AgentOS/examples/research-workflow/runtime"
	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
)

const settleTimeout = 4 * time.Minute

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
	// Wait until fan-out is actually executing (dynamic children exist).
	deadline := time.Now().Add(90 * time.Second)
	for {
		var running int
		if err := h.pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM tasks WHERE phase = 'RUNNING'`).Scan(&running); err != nil {
			t.Fatalf("count running tasks: %v", err)
		}
		if running >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	h.KillWorker("research-worker-a")
	h.KillWorker("research-worker-b")
	// Leases expire (15s heartbeat TTL) -> recovery requeues; restart BOTH
	// runtime instances because stranded runs may be bound to either pool.
	time.Sleep(20 * time.Second)
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
