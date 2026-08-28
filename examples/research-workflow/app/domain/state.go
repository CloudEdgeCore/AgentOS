package domain

import "strings"

// RunState is the application-layer research state machine of design doc §9.
// These states describe the research run for operators and the CLI; they are
// derived from the kernel WorkflowRun and its steps and are never written
// back into kernel state.
type RunState string

const (
	StateCreated       RunState = "CREATED"
	StatePlanning      RunState = "PLANNING"
	StateResearching   RunState = "RESEARCHING"
	StateAnalyzing     RunState = "ANALYZING"
	StateCritiquing    RunState = "CRITIQUING"
	StateWriting       RunState = "WRITING"
	StateValidating    RunState = "VALIDATING"
	StateCompleted     RunState = "COMPLETED"
	StateFailed        RunState = "FAILED"
	StateCancelled     RunState = "CANCELLED"
	StateBudgetExhaust RunState = "BUDGET_EXHAUSTED"
)

// Terminal reports whether the state ends the research run.
func (s RunState) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled, StateBudgetExhaust:
		return true
	}
	return false
}

// Kernel workflow/step status values (control-v1 contract).
const (
	kernelPending   = "PENDING"
	kernelRunning   = "RUNNING"
	kernelSucceeded = "SUCCEEDED"
	kernelFailed    = "FAILED"
	kernelCancelled = "CANCELLED"
)

// WorkflowBudgetExhaustedCode is the kernel failure code the orchestrator
// stamps when the workflow budget reservation loop rejects further work.
const WorkflowBudgetExhaustedCode = "WORKFLOW_BUDGET_EXHAUSTED"

// StepView is the slice of one workflow step the state machine needs.
type StepView struct {
	Name           string
	Status         string
	ParentStepName string
}

// WorkflowView is the slice of one kernel workflow the state machine needs.
type WorkflowView struct {
	Status      string
	FailureCode string
	Steps       []StepView
}

// DeriveRunState maps one kernel workflow observation onto the §9 state
// machine. The mapping is pure so the CLI, the API server and the tests all
// render identical statuses from identical observations.
func DeriveRunState(view WorkflowView) RunState {
	switch view.Status {
	case kernelPending:
		return StateCreated
	case kernelSucceeded:
		return StateCompleted
	case kernelFailed:
		if strings.EqualFold(view.FailureCode, WorkflowBudgetExhaustedCode) {
			return StateBudgetExhaust
		}
		return StateFailed
	case kernelCancelled:
		return StateCancelled
	}
	// Workflow is RUNNING: the deepest active stage wins. Static stage steps
	// are named by their role; dynamic research children carry a parent.
	var (
		validating, writing, critiquing bool
		analyzing, researching          bool
		planning                        bool
	)
	for _, step := range view.Steps {
		if step.Status != kernelRunning {
			continue
		}
		switch {
		case step.Name == "citation-validator":
			validating = true
		case step.Name == "writer":
			writing = true
		case strings.HasPrefix(step.Name, "critic"):
			critiquing = true
		case strings.HasPrefix(step.Name, "analyst"):
			analyzing = true
		case step.Name == "planner":
			planning = true
		default:
			// search-*/reader-* children and the collector routing hops are
			// the research phase.
			researching = true
		}
	}
	switch {
	case validating:
		return StateValidating
	case writing:
		return StateWriting
	case critiquing:
		return StateCritiquing
	case analyzing:
		return StateAnalyzing
	case researching:
		return StateResearching
	case planning:
		return StatePlanning
	}
	// Nothing is running right now (transitions between controllers, spawns
	// not yet dispatched, or a workflow whose planner has not started): the
	// run stays PLANNING until the first step makes progress.
	return StatePlanning
}
