package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeriveRunStateStaticLifecycle(t *testing.T) {
	running := func(step string) WorkflowView {
		return WorkflowView{Status: "RUNNING", Steps: []StepView{{Name: step, Status: "RUNNING"}}}
	}
	cases := []struct {
		name string
		view WorkflowView
		want RunState
	}{
		{"pending", WorkflowView{Status: "PENDING"}, StateCreated},
		{"succeeded", WorkflowView{Status: "SUCCEEDED"}, StateCompleted},
		{"cancelled", WorkflowView{Status: "CANCELLED"}, StateCancelled},
		{"failed", WorkflowView{Status: "FAILED", FailureCode: "TASK_FAILED"}, StateFailed},
		{"budget exhausted", WorkflowView{Status: "FAILED", FailureCode: "WORKFLOW_BUDGET_EXHAUSTED"}, StateBudgetExhaust},
		{"planner", running("planner"), StatePlanning},
		{"dynamic research child", WorkflowView{Status: "RUNNING", Steps: []StepView{
			{Name: "planner", Status: "SUCCEEDED"},
			{Name: "search-rq-001", Status: "RUNNING", ParentStepName: "planner"},
		}}, StateResearching},
		{"collector round", running("collector-r2"), StateResearching},
		{"analyst", running("analyst-r1"), StateAnalyzing},
		{"critic", running("critic-r1"), StateCritiquing},
		{"writer", running("writer"), StateWriting},
		{"validator", running("citation-validator"), StateValidating},
		// Deepest stage wins when two transitions race in one observation.
		{"writer beats analyst", WorkflowView{Status: "RUNNING", Steps: []StepView{
			{Name: "analyst-r3", Status: "SUCCEEDED"},
			{Name: "writer", Status: "RUNNING"},
			{Name: "citation-validator", Status: "RUNNING"},
		}}, StateValidating},
		// No step is running: the run stays in the earliest stage until the
		// first step makes progress.
		{"between controllers", WorkflowView{Status: "RUNNING"}, StatePlanning},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := DeriveRunState(testCase.view); got != testCase.want {
				t.Fatalf("DeriveRunState = %s, want %s", got, testCase.want)
			}
		})
	}
}

func TestRunStateTerminal(t *testing.T) {
	for _, state := range []RunState{StateCompleted, StateFailed, StateCancelled, StateBudgetExhaust} {
		if !state.Terminal() {
			t.Fatalf("%s must be terminal", state)
		}
	}
	for _, state := range []RunState{StateCreated, StatePlanning, StateResearching, StateAnalyzing, StateCritiquing, StateWriting, StateValidating} {
		if state.Terminal() {
			t.Fatalf("%s must not be terminal", state)
		}
	}
}

// TestResearchBudgetCalculation is the §16 "Research Budget Calculation"
// unit subject: the per-role table must sum to the documented intent and the
// hard limits must match design doc §10 exactly.
func TestResearchBudgetCalculation(t *testing.T) {
	if got, want := SumRoleBudgetTokens(), int64(126000); got != want {
		t.Fatalf("SumRoleBudgetTokens = %d, want %d", got, want)
	}
	if got, want := ResearchDecompositionBudget(), int64(20000); got != want {
		t.Fatalf("planner budget = %d, want %d", got, want)
	}
	defaults := DefaultWorkflowBudget()
	if defaults.MaxTasks != 80 || defaults.MaxTokens != 250000 ||
		defaults.MaxToolCalls != 250 || defaults.MaxCostMicroUSD != 5000000 {
		t.Fatalf("workflow budget defaults drifted from §10: %+v", defaults)
	}
	if PlannerMaxQuestions != 8 || SearchMaxSourcesPerQuesiton != 8 ||
		CriticMaxRounds != 3 || WorkflowMaxTasks != 80 {
		t.Fatalf("hard limits drifted from §10")
	}
	if ReaderMaxSourceBytes != 2<<20 {
		t.Fatalf("reader source limit = %d, want 2 MiB", ReaderMaxSourceBytes)
	}
	if err := ValidateRequestBudget(80, 250000); err != nil {
		t.Fatalf("doc-default budget rejected: %v", err)
	}
	if err := ValidateRequestBudget(81, 0); err == nil {
		t.Fatalf("maxTasks over the §10 hard limit must be rejected")
	}
	if err := ValidateRequestBudget(-1, 0); err == nil {
		t.Fatalf("negative maxTasks must be rejected")
	}
	if err := ValidateRequestBudget(0, -5); err == nil {
		t.Fatalf("negative maxTokens must be rejected")
	}
}

func TestDomainObjectsJSONRoundTrip(t *testing.T) {
	run := ResearchRun{ID: "research-001", WorkflowID: "wf", Goal: "g", Status: "RESEARCHING"}
	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{`"id"`, `"workflowId"`, `"goal"`, `"status"`, `"createdAt"`, `"completedAt"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("ResearchRun JSON missing %s: %s", field, encoded)
		}
	}
	evidence := Evidence{ID: "claim-001", SourceID: "src-001", QuestionID: "rq-001", Confidence: 0.91}
	encoded, err = json.Marshal(evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	var decoded Evidence
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded != evidence {
		t.Fatalf("evidence round trip mismatch: %+v", decoded)
	}
}
