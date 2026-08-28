package observability

import (
	"sort"
	"testing"
)

func TestPercentile(t *testing.T) {
	values := []float64{10, 20, 30, 40, 50}
	if got := Percentile(values, 50); got != 30 {
		t.Fatalf("P50 = %v, want 30", got)
	}
	if got := Percentile(values, 95); got != 50 {
		t.Fatalf("P95 = %v, want 50", got)
	}
	if got := Percentile(values, 99); got != 50 {
		t.Fatalf("P99 = %v, want 50", got)
	}
	if got := Percentile(nil, 50); got != 0 {
		t.Fatalf("empty P50 = %v, want 0", got)
	}
	sorted := []float64{5, 1, 3}
	sort.Float64s(sorted)
	if got := Percentile(sorted, 50); got != 3 {
		t.Fatalf("P50 of sorted = %v, want 3", got)
	}
}

func TestValidateCorrelation(t *testing.T) {
	valid := ExecutionCorrelation{
		Tasks:      []TaskNode{{ID: "t1", AgentVersionRef: "a@1", Phase: "SUCCEEDED"}},
		Steps:      []StepNode{{Name: "s1", TaskID: "t1", Status: "SUCCEEDED"}},
		Attempts:   []AttemptNode{{TaskID: "t1", Phase: "COMPLETED"}},
		ModelCalls: []ModelCallNode{{TaskID: "t1", Status: "COMPLETED"}},
		ToolCalls:  []ToolCallNode{{TaskID: "t1", Status: "EXECUTED"}},
	}
	if issues := ValidateCorrelation(valid); len(issues) != 0 {
		t.Fatalf("valid correlation rejected: %v", issues)
	}

	broken := ExecutionCorrelation{
		Tasks:    []TaskNode{{ID: "t1"}},
		Steps:    []StepNode{{Name: "s1", TaskID: "t-missing"}},
		Attempts: []AttemptNode{{TaskID: "t-missing"}},
		Memories: []MemoryNode{{SourceTaskID: "t-missing"}},
	}
	issues := ValidateCorrelation(broken)
	if len(issues) != 3 {
		t.Fatalf("broken correlation reported %d issues, want 3: %v", len(issues), issues)
	}
}
