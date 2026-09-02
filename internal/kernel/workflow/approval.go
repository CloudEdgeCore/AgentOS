package workflow

import (
	"context"

	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
)

// approvalRejected is the durable decision value that marks a rejected
// approval step.
const approvalRejected = "rejected"

// ApprovalController owns the human-approval gate for steps that require it:
// it parks undecided pending steps at WAITING_APPROVAL and records a
// rejected decision as SKIPPED.
type ApprovalController struct {
	workflows stepTransitioner
}

// NewApprovalController builds an approval gate bound to a step store.
func NewApprovalController(workflows stepTransitioner) *ApprovalController {
	return &ApprovalController{workflows: workflows}
}

// Park transitions an undecided pending approval-required step to
// WAITING_APPROVAL so it stays parked until a human decides. It reports
// whether the step was parked.
func (a *ApprovalController) Park(ctx context.Context, workflow kernelstore.Workflow, step kernelstore.WorkflowStep) (bool, error) {
	_, err := a.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
		TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
		ExpectedVersion: step.ResourceVersion, To: kernelstore.StepWaitingApproval,
	})
	return true, err
}

// Reject records a decided rejection as SKIPPED. It reports whether the step
// was rejected (and therefore must not be dispatched).
func (a *ApprovalController) Reject(ctx context.Context, workflow kernelstore.Workflow, step kernelstore.WorkflowStep) (bool, error) {
	if step.ApprovalDecision != approvalRejected {
		return false, nil
	}
	_, err := a.workflows.TransitionWorkflowStep(ctx, skipStepInput(workflow, step, "APPROVAL_REJECTED"))
	return true, err
}
