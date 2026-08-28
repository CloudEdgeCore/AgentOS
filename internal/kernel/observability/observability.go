// Package observability provides the read-side surface for the Phase 7
// observability contract: every execution exposes a full correlation chain
// (Workflow → Step → Task → Run → Attempt → Model/Tool Call → Memory →
// Result) plus the core metrics the platform guarantees.
package observability

import "time"

// ExecutionCorrelation is the full trace of one workflow execution.
type ExecutionCorrelation struct {
	Workflow    WorkflowNode
	Steps       []StepNode
	Tasks       []TaskNode
	Attempts    []AttemptNode
	ModelCalls  []ModelCallNode
	ToolCalls   []ToolCallNode
	Memories    []MemoryNode
	AuditEvents []AuditNode

	RuntimeOperationReceipts int
}

type WorkflowNode struct {
	ID, Status, Goal       string
	CreatedAt, CompletedAt time.Time
}

type StepNode struct {
	Name, Status, TaskID, FailureCode string
	AttemptCount                      int
}

type TaskNode struct {
	ID, AgentVersionRef, Phase, ResultRef string
	CreatedAt                             time.Time
}

type AttemptNode struct {
	TaskID, RunID                                       string
	Ordinal, RunOrdinal                                 int
	Phase, RuntimeClass, RuntimeInstanceID, FailureCode string
	FencingToken                                        int64
	CreatedAt                                           time.Time
}

type ModelCallNode struct {
	TaskID, RunID, AttemptID, ModelRef, Status string
}

type ToolCallNode struct {
	TaskID, RunID, AttemptID, ToolName, Action, Status string
}

type MemoryNode struct {
	Namespace, Key                string
	SourceTaskID, SourceAttemptID string
}

type AuditNode struct {
	EventType, ResourceType, ResourceID, Actor, Details string
	OccurredAt                                          time.Time
}

// ValidateCorrelation asserts the chain invariants: every step's task exists
// among the tasks; every attempt's task exists; every model/tool call links
// to a task; every memory record links to a task or attempt. Returns a list
// of broken invariants (empty = valid).
func ValidateCorrelation(c ExecutionCorrelation) []string {
	var issues []string
	taskIDs := map[string]bool{}
	for _, task := range c.Tasks {
		taskIDs[task.ID] = true
	}
	stepTaskIDs := map[string]string{}
	for _, step := range c.Steps {
		if step.TaskID != "" {
			stepTaskIDs[step.TaskID] = step.Name
			if !taskIDs[step.TaskID] {
				issues = append(issues, "step "+step.Name+" references task "+step.TaskID+" not in task list")
			}
		}
	}
	for _, attempt := range c.Attempts {
		if !taskIDs[attempt.TaskID] {
			issues = append(issues, "attempt references task "+attempt.TaskID+" not in task list")
		}
	}
	for _, mc := range c.ModelCalls {
		if !taskIDs[mc.TaskID] {
			issues = append(issues, "model call references task "+mc.TaskID+" not in task list")
		}
	}
	for _, tc := range c.ToolCalls {
		if !taskIDs[tc.TaskID] {
			issues = append(issues, "tool call references task "+tc.TaskID+" not in task list")
		}
	}
	for _, mem := range c.Memories {
		if mem.SourceTaskID != "" && !taskIDs[mem.SourceTaskID] {
			issues = append(issues, "memory record references task "+mem.SourceTaskID+" not in task list")
		}
	}
	return issues
}

// Metrics is the aggregated platform health surface (§Phase-7 core metrics).
type Metrics struct {
	WorkflowCount, WorkflowSucceeded, WorkflowFailed int
	TaskCount, TaskSucceeded, TaskFailed             int
	ModelCalls, ToolCalls, MemoryRecords             int
	AuditEvents, Receipts                            int

	WorkflowSuccessRate, TaskSuccessRate float64
	SchedulingLatencyMillis              Percentiles
	RetryRate, RecoveryRate              float64

	BudgetDrift           bool // reserved != settled tokens
	CapacityDrift         int  // ACTIVE reservations remaining
	DuplicateSideEffects  int  // tool_calls EXECUTED more than once for the same (task, tool, args)
	CrossTenantViolations int  // rows in other tenants referencing this workflow's resources
}

type Percentiles struct {
	P50, P95, P99 float64
}

// Percentile computes the p-th percentile of sorted values (0 ≤ p ≤ 100).
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	index := int(float64(len(values)) * p / 100.0)
	if index >= len(values) {
		index = len(values) - 1
	}
	if index < 0 {
		index = 0
	}
	return values[index]
}
