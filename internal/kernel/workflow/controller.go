// The orchestrator controller. Reconcile is stateless and durable: all
// state lives in PostgreSQL, so restarts (and concurrent orchestrator
// instances) resume without losing dependency progress. Task creation is
// idempotent per (workflow, step, attempt) and every step/workflow
// transition is CAS-guarded, so racing instances converge with zero double
// dispatch.
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/agentmetrics"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/errorcode"
	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var ErrOutputContract = errors.New("workflow output contract violation")

// TaskPipeline is the task surface the orchestrator drives. The kernel
// postgres Store satisfies it; every step becomes an ordinary fenced Task.
type TaskPipeline interface {
	CreateTask(context.Context, kernelstore.CreateTaskInput) (kernelstore.CreateTaskResult, error)
	GetTask(context.Context, string, uuid.UUID) (kernelstore.Task, error)
	RequestTaskCancellation(context.Context, string, uuid.UUID, int64) (kernelstore.Task, error)
}

// ResultReader opens task result artifacts to extract dependency outputs
// for condition evaluation and downstream goals. The artifact.Filesystem
// satisfies it.
type ResultReader interface {
	Open(context.Context, string, kernelstore.ArtifactReference) (io.ReadCloser, error)
}

type workflowClaimRenewer interface {
	RenewWorkflowClaim(context.Context, string, uuid.UUID, string, time.Duration) error
}

// Controller reconciles active workflows.
type Controller struct {
	workflows kernelstore.WorkflowStore
	tasks     TaskPipeline
	artifacts ResultReader
	owner     string
	batch     int
	parallel  int
	// maxInFlight bounds the concurrently dispatched (task-carrying) steps
	// of one workflow: a 1,000-step fan-out must flood neither the
	// placement queue nor the runtime fleet.
	maxInFlight int
	// claimLease bounds one instance's exclusive claim of a workflow
	// Zero reconciles without claiming (single-instance mode).
	claimLease time.Duration
	// claimTokenBudget skips claiming workflows whose settled token usage
	// already exceeds their token ceiling (they are budget-stopped).
	claimTokenBudget int64
	now              func() time.Time
	newID            func() uuid.UUID
	logger           *slog.Logger
}

// NewController builds the orchestrator. batch bounds workflows per
// reconcile; parallel bounds concurrent per-workflow processing (default 4).
func NewController(workflows kernelstore.WorkflowStore, tasks TaskPipeline, artifacts ResultReader, owner string, batch int) *Controller {
	return &Controller{
		workflows: workflows, tasks: tasks, artifacts: artifacts, owner: owner,
		batch: batch, parallel: 4, maxInFlight: defaultMaxInFlightSteps,
		now: func() time.Time { return time.Now().UTC() },
		newID: func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		},
		logger: slog.Default(),
	}
}

// WithParallelism bounds concurrent per-workflow reconciliation.
func (c *Controller) WithParallelism(workers int) *Controller {
	if workers > 0 {
		c.parallel = workers
	}
	return c
}

// WithClaiming enables distributed work-claim sharding: each
// reconcile claims a batch of workflows under a lease that expires when the
// instance dies, so peers take over without double dispatch. A zero lease
// keeps the single-instance behavior (reconcile everything visible).
func (c *Controller) WithClaiming(lease time.Duration, tokenBudget int64) *Controller {
	if lease > 0 {
		c.claimLease = lease
		c.claimTokenBudget = tokenBudget
	}
	return c
}

// defaultMaxInFlightSteps caps dispatched-but-unfinished steps per workflow.
const defaultMaxInFlightSteps = 64

// WithMaxInFlightSteps bounds concurrently dispatched steps per workflow.
func (c *Controller) WithMaxInFlightSteps(limit int) *Controller {
	if limit > 0 {
		c.maxInFlight = limit
	}
	return c
}

// Reconcile drives active workflows one round: dispatching ready steps,
// observing task terminals, applying retries, propagating cancellation,
// enforcing workflow budgets and finalizing workflows. In claiming mode
// Each instance leases its batch under an expiring claim; otherwise it
// reconciles everything visible. Retryable transaction
// conflicts are retried with bounded backoff (ADR-002).
func (c *Controller) Reconcile(ctx context.Context) (int, error) {
	var (
		active []kernelstore.Workflow
		err    error
	)
	if c.claimLease > 0 {
		active, err = c.workflows.ClaimWorkflows(ctx, kernelstore.ClaimWorkflowsInput{
			Owner: c.owner, Batch: c.batch, Lease: c.claimLease, MaxTokens: c.claimTokenBudget,
		})
	} else {
		active, err = c.workflows.ListActiveWorkflows(ctx, c.batch)
	}
	if err != nil {
		return 0, err
	}
	agentmetrics.WorkflowClaims(ctx, len(active))
	agentmetrics.QueueDepth(ctx, "orchestrator_claim_batch", int64(len(active)))
	if len(active) == 0 {
		return 0, nil
	}
	processed := 0
	if c.parallel <= 1 || len(active) == 1 {
		for _, workflow := range active {
			ok, err := c.processClaimedWorkflow(ctx, workflow)
			if err != nil {
				return processed, err
			}
			if ok {
				processed++
				agentmetrics.WorkflowOutcome(ctx, "progressed")
			}
		}
		return processed, nil
	}
	var mu sync.Mutex
	var batchErr error
	semaphore := make(chan struct{}, c.parallel)
	var wg sync.WaitGroup
	for _, workflow := range active {
		workflow := workflow
		mu.Lock()
		aborted := batchErr != nil
		mu.Unlock()
		if aborted {
			break
		}
		wg.Add(1)
		semaphore <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			ok, err := c.processClaimedWorkflow(ctx, workflow)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				agentmetrics.WorkflowOutcome(ctx, "reconcile_error")
				if batchErr == nil {
					batchErr = err
				}
				return
			}
			if ok {
				processed++
				agentmetrics.WorkflowOutcome(ctx, "progressed")
			}
		}()
	}
	wg.Wait()
	return processed, batchErr
}

func (c *Controller) processClaimedWorkflow(ctx context.Context, workflow kernelstore.Workflow) (bool, error) {
	renewer, ok := c.workflows.(workflowClaimRenewer)
	if !ok || c.claimLease <= 0 {
		return c.processWorkflow(ctx, workflow)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	renewErr := make(chan error, 1)
	interval := c.claimLease / 3
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := renewer.RenewWorkflowClaim(workCtx, workflow.TenantID, workflow.ID, c.owner, c.claimLease); err != nil {
					select {
					case renewErr <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	processed, err := c.processWorkflow(workCtx, workflow)
	cancel()
	select {
	case claimErr := <-renewErr:
		if err != nil {
			return processed, errors.Join(err, claimErr)
		}
		return processed, fmt.Errorf("renew workflow claim: %w", claimErr)
	default:
		return processed, err
	}
}

// processWorkflow advances one workflow; it reports whether state moved.
func (c *Controller) processWorkflow(ctx context.Context, workflow kernelstore.Workflow) (bool, error) {
	steps, err := c.workflows.ListWorkflowSteps(ctx, workflow.TenantID, workflow.ID)
	if err != nil {
		return false, err
	}
	byName := make(map[string]kernelstore.WorkflowStep, len(steps))
	for index := range steps {
		step := steps[index]
		byName[step.Name] = step
	}
	spec, err := decodeStoredSpec(workflow.Spec)
	if err != nil {
		// A persisted spec that no longer decodes is a kernel bug; fail the
		// workflow loudly rather than looping forever.
		_, failErr := c.workflows.TransitionWorkflow(ctx, kernelstore.TransitionWorkflowInput{
			TenantID: workflow.TenantID, WorkflowID: workflow.ID, ExpectedVersion: workflow.ResourceVersion,
			To: kernelstore.WorkflowFailed, FailureCode: "WORKFLOW_SPEC_INVALID",
		})
		if failErr != nil {
			return false, errors.Join(err, failErr)
		}
		return true, nil
	}
	moved := false

	// PENDING workflows start once their steps exist (creation is atomic,
	// so this always succeeds on the first reconcile).
	if workflow.Status == kernelstore.WorkflowPending {
		updated, err := c.workflows.TransitionWorkflow(ctx, kernelstore.TransitionWorkflowInput{
			TenantID: workflow.TenantID, WorkflowID: workflow.ID, ExpectedVersion: workflow.ResourceVersion,
			To: kernelstore.WorkflowRunning,
		})
		if err != nil {
			if errors.Is(err, kernelstore.ErrVersionConflict) {
				return false, nil // a concurrent instance advanced it
			}
			return false, err
		}
		workflow = updated
		moved = true
	}

	// Cancellation propagation: durable intent first, then drain active
	// steps and finalize.
	if workflow.CancelRequestedAt != nil {
		done, err := c.cancelWorkflow(ctx, workflow, steps)
		if err != nil {
			return moved, err
		}
		return moved || done, nil
	}

	// Workflow deadline hard stop. Persist the stop marker before any
	// cancellation so another orchestrator can safely resume the drain.
	if workflow.DeadlineExceededAt == nil && workflow.DeadlineAt != nil && !c.now().Before(*workflow.DeadlineAt) {
		if _, err := c.workflows.MarkWorkflowDeadlineExceeded(ctx, workflow.TenantID, workflow.ID, workflow.ResourceVersion); err != nil {
			if errors.Is(err, kernelstore.ErrVersionConflict) {
				return moved, nil
			}
			return moved, err
		}
		return true, nil
	}
	if workflow.DeadlineExceededAt != nil {
		done, err := c.drainFailedWorkflow(ctx, workflow, steps, errorcode.WorkflowDeadlineExceeded)
		if err != nil {
			return moved, err
		}
		return moved || done, nil
	}

	// Workflow budget hard stop: once a ceiling is met, the workflow
	// drains exactly like a cancellation - running steps finish or are
	// cancelled, undispatched steps are skipped - and finalizes FAILED with
	// WORKFLOW_BUDGET_EXHAUSTED. The durable marker (budget_exhausted_at)
	// makes the stop survive restarts and racing orchestrators.
	if workflow.BudgetExhaustedAt == nil {
		if exhausted := c.checkBudget(ctx, workflow); exhausted {
			return true, nil
		}
	} else {
		done, err := c.drainBudgetExhausted(ctx, workflow, steps)
		if err != nil {
			return moved, err
		}
		return moved || done, nil
	}

	inFlight := 0
	for _, step := range steps {
		if step.Status == kernelstore.StepRunning {
			inFlight++
		}
	}
	for index := range steps {
		step := steps[index]
		if step.Status.Terminal() {
			continue
		}
		// Backpressure: once the in-flight window is full, further ready
		// steps wait for their siblings to drain (declaration order).
		if step.Status == kernelstore.StepPending && inFlight >= c.maxInFlight {
			break
		}
		wasPending := step.Status == kernelstore.StepPending
		changed, err := c.advanceStep(ctx, workflow, step, byName, spec)
		if err != nil {
			return moved, err
		}
		if changed && wasPending {
			inFlight++
		}
		moved = moved || changed
		if changed {
			// Keep dependency decisions in this reconcile round aligned with
			// durable state instead of waiting for the next controller tick.
			// Refresh only the changed row: rescanning and sorting the entire
			// DAG per transition makes a 10k-step workflow quadratic.
			latest, getErr := c.workflows.GetWorkflowStep(ctx, workflow.TenantID, workflow.ID, step.Name)
			if getErr != nil {
				return moved, getErr
			}
			steps[index] = latest
			byName[latest.Name] = latest
		}
	}

	// Finalize when every step is terminal.
	allTerminal := true
	failed, cancelled := false, false
	for _, step := range steps {
		if !step.Status.Terminal() {
			allTerminal = false
			break
		}
		switch step.Status {
		case kernelstore.StepFailed:
			failed = true
		case kernelstore.StepCancelled:
			cancelled = true
		}
	}
	if allTerminal {
		target := kernelstore.WorkflowSucceeded
		code := ""
		switch {
		case failed:
			target, code = kernelstore.WorkflowFailed, "WORKFLOW_STEP_FAILED"
		case cancelled:
			target, code = kernelstore.WorkflowCancelled, "WORKFLOW_CANCELLED"
		}
		if _, err := c.workflows.TransitionWorkflow(ctx, kernelstore.TransitionWorkflowInput{
			TenantID: workflow.TenantID, WorkflowID: workflow.ID, ExpectedVersion: workflow.ResourceVersion,
			To: target, FailureCode: code,
		}); err != nil {
			if errors.Is(err, kernelstore.ErrVersionConflict) {
				return moved, nil
			}
			return moved, err
		}
		moved = true
	}
	return moved, nil
}

// advanceStep moves one non-terminal step forward.
func (c *Controller) advanceStep(ctx context.Context, workflow kernelstore.Workflow, step kernelstore.WorkflowStep, byName map[string]kernelstore.WorkflowStep, spec WorkflowSpec) (bool, error) {
	switch step.Status {
	case kernelstore.StepPending:
		return c.dispatchStep(ctx, workflow, step, byName, spec)
	case kernelstore.StepWaitingApproval:
		if step.DecidedBy == "" {
			return false, nil // parked until the human decides
		}
		declared := declaredStep(spec, step.Name)
		if declared == nil {
			return false, fmt.Errorf("step %q missing from stored spec", step.Name)
		}
		if declared.RequiresApproval && step.ApprovalDecision == approvalRejected {
			_, err := c.workflows.TransitionWorkflowStep(ctx, skipStepInput(workflow, step, "APPROVAL_REJECTED"))
			return true, err
		}
		return c.dispatchStep(ctx, workflow, step, byName, spec)
	case kernelstore.StepRunning:
		return c.observeStep(ctx, workflow, step, spec)
	default:
		return false, nil
	}
}

const approvalRejected = "rejected"

// dispatchStep checks dependencies, conditions and approval, then creates
// the step's Task.
func (c *Controller) dispatchStep(ctx context.Context, workflow kernelstore.Workflow, step kernelstore.WorkflowStep, byName map[string]kernelstore.WorkflowStep, spec WorkflowSpec) (bool, error) {
	declared := declaredStep(spec, step.Name)
	// Dependency gating (join semantics: every dependency must succeed).
	// "spawn:<parent>" (dynamic steps only) matches every dynamic child of
	// the parent, with the same join semantics per member.
	var dependencyOutputs map[string]string
	if step.IsDynamic {
		dependencyOutputs = make(map[string]string)
	} else {
		dependencyOutputs = make(map[string]string, len(declared.DependsOn))
	}
	for _, dependency := range stepDependencies(step, declared) {
		if parentName, group := strings.CutPrefix(dependency, "spawn:"); group {
			parent, exists := byName[parentName]
			if !exists {
				return false, fmt.Errorf("dynamic group parent %q is missing", parentName)
			}
			switch parent.Status {
			case kernelstore.StepSucceeded:
				// The spawning attempt is terminal, so the child set is now
				// closed and can safely be joined (including an empty set).
			case kernelstore.StepFailed, kernelstore.StepSkipped, kernelstore.StepCancelled:
				_, err := c.workflows.TransitionWorkflowStep(ctx, skipStepInput(workflow, step, "UPSTREAM_NOT_SUCCEEDED"))
				return true, err
			default:
				return false, nil
			}
		}
		upstreams := resolveDependencySteps(dependency, step, byName)
		if len(upstreams) == 0 && isGroupDependency(dependency) {
			// The group is empty: no dynamic children were ever spawned, so
			// the dependency is vacuously satisfied.
			continue
		}
		for _, upstream := range upstreams {
			switch upstream.Status {
			case kernelstore.StepSucceeded:
				dependencyOutputs[upstream.Name] = resultSummaryOutput(upstream.ResultSummary)
			case kernelstore.StepFailed, kernelstore.StepSkipped, kernelstore.StepCancelled:
				// A failed upstream never executes its dependents.
				_, err := c.workflows.TransitionWorkflowStep(ctx, skipStepInput(workflow, step, "UPSTREAM_NOT_SUCCEEDED"))
				return true, err
			default:
				return false, nil // still waiting on the dependency
			}
		}
	}
	// Condition evaluation against the stored dependency output.
	if condition := stepCondition(step, declared); condition != nil {
		output := dependencyOutputs[condition.Step]
		met := false
		switch {
		case condition.OutputContains != "":
			met = containsBounded(output, condition.OutputContains)
		case condition.OutputEquals != "":
			met = output == condition.OutputEquals
		case condition.JSONPointer != "":
			var upstream any
			if err := json.Unmarshal([]byte(output), &upstream); err == nil {
				actual, ok := resolveJSONPointer(upstream, condition.JSONPointer)
				var expected any
				if ok && json.Unmarshal(condition.EqualsJSON, &expected) == nil {
					met = reflect.DeepEqual(actual, expected)
				}
			}
		}
		if !met {
			_, err := c.workflows.TransitionWorkflowStep(ctx, skipStepInput(workflow, step, "CONDITION_NOT_MET"))
			return true, err
		}
	}
	if requiresApproval(step, declared) && step.DecidedBy == "" && step.Status == kernelstore.StepPending {
		_, err := c.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
			TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
			ExpectedVersion: step.ResourceVersion, To: kernelstore.StepWaitingApproval,
		})
		return true, err
	}

	// The stored spec is the raw document; re-apply the default/overlay
	// merge exactly as publication validation did. Dynamic steps carry
	// their merged spec inline (spawned through the broker, after the
	// same merge).
	var (
		agentVersionRef string
		goal            string
		mergedSpec      json.RawMessage
	)
	if step.IsDynamic {
		agentVersionRef, goal, mergedSpec = step.AgentVersionRef, step.Goal, step.Spec
	} else {
		var err error
		mergedSpec, err = mergeSpecs(objectMap(spec.DefaultTaskSpec), objectMap(declared.Spec))
		if err != nil {
			return false, fmt.Errorf("step %q task spec: %w", step.Name, err)
		}
		agentVersionRef, goal = declared.AgentVersionRef, declared.Goal
	}

	// Idempotent per (workflow, step, attempt): racing orchestrators create
	// exactly one Task.
	attempt := step.AttemptCount + 1
	created, err := c.tasks.CreateTask(ctx, kernelstore.CreateTaskInput{
		ID: c.newID(), TenantID: workflow.TenantID, Namespace: workflow.Namespace,
		AgentVersionRef: agentVersionRef, Goal: renderGoal(goal, dependencyOutputs),
		Spec: mergedSpec, IdempotencyKey: fmt.Sprintf("workflow/%s/%s/%d", workflow.ID, step.Name, attempt),
		WorkflowID: &workflow.ID, WorkflowStepID: &step.ID, WorkflowStepName: step.Name,
		WorkflowAttempt: attempt, ParentTaskID: parentTaskID(step, byName),
	})
	if err != nil {
		return false, fmt.Errorf("create task for step %q: %w", step.Name, err)
	}
	taskID := created.Task.ID
	nextAttempt := attempt
	_, err = c.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
		TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
		ExpectedVersion: step.ResourceVersion, To: kernelstore.StepRunning,
		TaskID: &taskID, AttemptCount: &nextAttempt,
	})
	if err != nil {
		if errors.Is(err, kernelstore.ErrVersionConflict) {
			return false, nil // a concurrent instance dispatched it
		}
		return false, err
	}
	return true, nil
}

func parentTaskID(step kernelstore.WorkflowStep, byName map[string]kernelstore.WorkflowStep) *uuid.UUID {
	if !step.IsDynamic || step.ParentStepName == "" {
		return nil
	}
	parent, ok := byName[step.ParentStepName]
	if !ok || parent.TaskID == nil {
		return nil
	}
	id := *parent.TaskID
	return &id
}

// stepDependencies returns the dependency names of one step: a dynamic
// child waits for the exact step that spawned it, while a declared step may
// use "spawn:<parent>" to join every child spawned by that parent.
func stepDependencies(step kernelstore.WorkflowStep, declared *StepSpec) []string {
	if step.IsDynamic {
		if step.ParentStepName != "" {
			return []string{step.ParentStepName}
		}
		return nil
	}
	if declared == nil {
		return nil
	}
	return declared.DependsOn
}

// isGroupDependency reports whether a dependency token names a spawn group.
func isGroupDependency(dependency string) bool {
	return strings.HasPrefix(dependency, "spawn:")
}

// resolveDependencySteps maps one dependency token to its step records: the
// named step, or every dynamic child of the group parent.
func resolveDependencySteps(dependency string, step kernelstore.WorkflowStep, byName map[string]kernelstore.WorkflowStep) []kernelstore.WorkflowStep {
	if parent, ok := strings.CutPrefix(dependency, "spawn:"); ok {
		children := make([]kernelstore.WorkflowStep, 0, 4)
		for _, candidate := range byName {
			if candidate.ParentStepName == parent {
				children = append(children, candidate)
			}
		}
		return children
	}
	if upstream, ok := byName[dependency]; ok {
		return []kernelstore.WorkflowStep{upstream}
	}
	return nil
}

// stepCondition returns the effective condition of one step.
func stepCondition(step kernelstore.WorkflowStep, declared *StepSpec) *StepCondition {
	if declared == nil {
		return nil
	}
	return declared.Condition
}

// requiresApproval reports whether one step parks for human approval.
func requiresApproval(step kernelstore.WorkflowStep, declared *StepSpec) bool {
	return declared != nil && declared.RequiresApproval
}

// observeStep reacts to the step task's terminal phase: extract the result,
// retry within the budget, or fail the step.
func (c *Controller) observeStep(ctx context.Context, workflow kernelstore.Workflow, step kernelstore.WorkflowStep, spec WorkflowSpec) (bool, error) {
	if step.TaskID == nil {
		return false, fmt.Errorf("running step %q has no task", step.Name)
	}
	task, err := c.tasks.GetTask(ctx, workflow.TenantID, *step.TaskID)
	if err != nil {
		return false, err
	}
	if !task.Phase.Terminal() {
		return false, nil
	}
	switch task.Phase {
	case "SUCCEEDED":
		var outputContract *StepOutputContract
		if declared := declaredStep(spec, step.Name); declared != nil {
			outputContract = declared.Output
		}
		summary, extractErr := c.extractResult(ctx, workflow.TenantID, task, outputContract)
		if extractErr != nil {
			c.logger.Warn("workflow step result extraction failed", "step", step.Name, "error", extractErr)
			if errors.Is(extractErr, ErrOutputContract) {
				_, err := c.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
					TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
					ExpectedVersion: step.ResourceVersion, To: kernelstore.StepFailed,
					FailureCode: errorcode.OutputContractViolation,
				})
				return true, err
			}
			// Result propagation is part of step success. Leave the step
			// RUNNING so a later reconcile retries the artifact read; marking
			// success here would release downstream dependencies with no data.
			return false, nil
		}
		_, err := c.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
			TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
			ExpectedVersion: step.ResourceVersion, To: kernelstore.StepSucceeded, ResultSummary: summary,
		})
		if err != nil && errors.Is(err, kernelstore.ErrVersionConflict) {
			return false, nil
		}
		return true, err
	case "CANCELLED", "TIMED_OUT", "REJECTED":
		_, err := c.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
			TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
			ExpectedVersion: step.ResourceVersion, To: kernelstore.StepCancelled,
			FailureCode: "TASK_" + string(task.Phase),
		})
		if err != nil && errors.Is(err, kernelstore.ErrVersionConflict) {
			return false, nil
		}
		return true, err
	default: // FAILED
		maxAttempts := 1
		if step.IsDynamic {
			maxAttempts = step.MaxAttempts
			if maxAttempts <= 0 {
				maxAttempts = 1
			}
		} else if declared := declaredStep(spec, step.Name); declared != nil && declared.Retry != nil {
			maxAttempts = declared.Retry.MaxAttempts
		}
		if step.AttemptCount < maxAttempts {
			// Single-step retry: back to PENDING; completed siblings stay.
			_, err := c.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
				TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
				ExpectedVersion: step.ResourceVersion, To: kernelstore.StepPending,
				FailureCode: "RETRY_AFTER_TASK_FAILED",
			})
			if err != nil && errors.Is(err, kernelstore.ErrVersionConflict) {
				return false, nil
			}
			return true, err
		}
		_, err := c.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
			TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
			ExpectedVersion: step.ResourceVersion, To: kernelstore.StepFailed,
			FailureCode: "TASK_FAILED",
		})
		if err != nil && errors.Is(err, kernelstore.ErrVersionConflict) {
			return false, nil
		}
		return true, err
	}
}

// checkBudget evaluates the workflow ceilings against the aggregated usage
// and records the durable stop marker when one is met. It reports whether
// the stop was recorded this round.
func (c *Controller) checkBudget(ctx context.Context, workflow kernelstore.Workflow) bool {
	if workflow.BudgetMaxTasks == 0 && workflow.BudgetMaxTokens == 0 && workflow.BudgetMaxCostMicroUSD == 0 {
		return false
	}
	usage, err := c.workflows.WorkflowUsageSnapshot(ctx, workflow.TenantID, workflow.ID)
	if err != nil {
		c.logger.Warn("workflow usage snapshot failed", "workflow", workflow.ID, "error", err)
		return false
	}
	if !usage.Exhausted() {
		return false
	}
	if _, err := c.workflows.MarkWorkflowBudgetExhausted(ctx, workflow.TenantID, workflow.ID, workflow.ResourceVersion); err != nil {
		if !errors.Is(err, kernelstore.ErrVersionConflict) {
			c.logger.Warn("mark workflow budget exhausted", "workflow", workflow.ID, "error", err)
		}
		return false
	}
	c.logger.Info("workflow budget exhausted", "workflow", workflow.ID,
		"tasks", usage.Tasks, "tokens", usage.Tokens, "costUsd", usage.CostMicroUSD.USD())
	return true
}

// drainBudgetExhausted drains a budget-stopped workflow: running step tasks
// are cancelled, undispatched steps are skipped, and the workflow finalizes
// FAILED (WORKFLOW_BUDGET_EXHAUSTED) once nothing is running. A step that
// already succeeded keeps its result.
func (c *Controller) drainBudgetExhausted(ctx context.Context, workflow kernelstore.Workflow, steps []kernelstore.WorkflowStep) (bool, error) {
	return c.drainFailedWorkflow(ctx, workflow, steps, errorcode.WorkflowBudgetExhausted)
}

// drainFailedWorkflow cancels active tasks, skips undispatched steps, and
// finalizes a workflow with a durable failure reason.
func (c *Controller) drainFailedWorkflow(ctx context.Context, workflow kernelstore.Workflow, steps []kernelstore.WorkflowStep, failureCode string) (bool, error) {
	moved := false
	for _, step := range steps {
		if step.Status.Terminal() {
			continue
		}
		if step.Status == kernelstore.StepRunning && step.TaskID != nil {
			task, err := c.tasks.GetTask(ctx, workflow.TenantID, *step.TaskID)
			if err != nil {
				return moved, err
			}
			if !task.Phase.Terminal() {
				if _, err := c.tasks.RequestTaskCancellation(ctx, workflow.TenantID, task.ID, task.ResourceVersion); err != nil {
					if errors.Is(err, kernelstore.ErrVersionConflict) {
						continue
					}
					return moved, err
				}
				moved = true
				continue
			}
			target, code := kernelstore.StepCancelled, failureCode
			if task.Phase == "SUCCEEDED" {
				target, code = kernelstore.StepSucceeded, ""
			}
			_, err = c.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
				TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
				ExpectedVersion: step.ResourceVersion, To: target, FailureCode: code,
			})
			if err != nil && !errors.Is(err, kernelstore.ErrVersionConflict) {
				return moved, err
			}
			moved = true
			continue
		}
		_, err := c.workflows.TransitionWorkflowStep(ctx, skipStepInput(workflow, step, failureCode))
		if err != nil {
			if errors.Is(err, kernelstore.ErrVersionConflict) {
				continue
			}
			return moved, err
		}
		moved = true
	}
	current, err := c.workflows.ListWorkflowSteps(ctx, workflow.TenantID, workflow.ID)
	if err != nil {
		return moved, err
	}
	for _, step := range current {
		if !step.Status.Terminal() {
			return moved, nil
		}
	}
	if workflow.Status != kernelstore.WorkflowFailed {
		if _, err := c.workflows.TransitionWorkflow(ctx, kernelstore.TransitionWorkflowInput{
			TenantID: workflow.TenantID, WorkflowID: workflow.ID, ExpectedVersion: workflow.ResourceVersion,
			To: kernelstore.WorkflowFailed, FailureCode: failureCode,
		}); err != nil && !errors.Is(err, kernelstore.ErrVersionConflict) {
			return moved, err
		}
		return true, nil
	}
	return moved, nil
}

// cancelWorkflow propagates the durable cancel intent: active step tasks
// are cancelled through the kernel, undischarged steps are skipped, and the
// workflow finalizes CANCELLED once nothing is running.
func (c *Controller) cancelWorkflow(ctx context.Context, workflow kernelstore.Workflow, steps []kernelstore.WorkflowStep) (bool, error) {
	moved := false
	for _, step := range steps {
		if step.Status.Terminal() {
			continue
		}
		if step.Status == kernelstore.StepRunning && step.TaskID != nil {
			task, err := c.tasks.GetTask(ctx, workflow.TenantID, *step.TaskID)
			if err != nil {
				return moved, err
			}
			if !task.Phase.Terminal() {
				if _, err := c.tasks.RequestTaskCancellation(ctx, workflow.TenantID, task.ID, task.ResourceVersion); err != nil {
					if errors.Is(err, kernelstore.ErrVersionConflict) {
						continue // already moving; next round observes it
					}
					return moved, err
				}
				moved = true
				continue
			}
			// The task reached a terminal phase during cancellation: record
			// the observed outcome (success stays success).
			target, code := kernelstore.StepCancelled, "WORKFLOW_CANCELLED"
			if task.Phase == "SUCCEEDED" {
				target, code = kernelstore.StepSucceeded, ""
			}
			_, err = c.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
				TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
				ExpectedVersion: step.ResourceVersion, To: target, FailureCode: code,
			})
			if err != nil && !errors.Is(err, kernelstore.ErrVersionConflict) {
				return moved, err
			}
			moved = true
			continue
		}
		_, err := c.workflows.TransitionWorkflowStep(ctx, skipStepInput(workflow, step, "WORKFLOW_CANCELLED"))
		if err != nil {
			if errors.Is(err, kernelstore.ErrVersionConflict) {
				continue
			}
			return moved, err
		}
		moved = true
	}
	// Finalize when the drain finished.
	steps, err := c.workflows.ListWorkflowSteps(ctx, workflow.TenantID, workflow.ID)
	if err != nil {
		return moved, err
	}
	for _, step := range steps {
		if !step.Status.Terminal() {
			return moved, nil
		}
	}
	if workflow.Status != kernelstore.WorkflowCancelled {
		if _, err := c.workflows.TransitionWorkflow(ctx, kernelstore.TransitionWorkflowInput{
			TenantID: workflow.TenantID, WorkflowID: workflow.ID, ExpectedVersion: workflow.ResourceVersion,
			To: kernelstore.WorkflowCancelled, FailureCode: "WORKFLOW_CANCELLED",
		}); err != nil && !errors.Is(err, kernelstore.ErrVersionConflict) {
			return moved, err
		}
		return true, nil
	}
	return moved, nil
}

// extractResult reads the task result artifact and stores a bounded output
// summary for conditions and downstream goals.
func (c *Controller) extractResult(ctx context.Context, tenantID string, task kernelstore.Task, contract *StepOutputContract) (json.RawMessage, error) {
	if task.ResultRef == "" {
		return nil, fmt.Errorf("succeeded task has no result artifact")
	}
	digest, size, mediaType, err := c.workflows.ArtifactMetadata(ctx, tenantID, task.ResultRef)
	if err != nil {
		return nil, err
	}
	reference := kernelstore.ArtifactReference{URI: task.ResultRef, MediaType: mediaType, SizeBytes: size}
	copy(reference.SHA256[:], digest)
	reader, err := c.artifacts.Open(ctx, tenantID, reference)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode result document: %w", err)
	}
	output := document["output"]
	if contract != nil {
		if contract.ContentType != "" && mediaType != "" && !strings.HasPrefix(mediaType, contract.ContentType) {
			return nil, fmt.Errorf("%w: content type %q does not match %q", ErrOutputContract, mediaType, contract.ContentType)
		}
		var schemaDocument any
		if err := json.Unmarshal(contract.Schema, &schemaDocument); err != nil {
			return nil, fmt.Errorf("%w: invalid stored schema", ErrOutputContract)
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("workflow-output.json", schemaDocument); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrOutputContract, err)
		}
		compiled, err := compiler.Compile("workflow-output.json")
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrOutputContract, err)
		}
		if err := compiled.Validate(output); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrOutputContract, err)
		}
	}
	encodedOutput, err := json.Marshal(output)
	if err != nil || len(encodedOutput) > 64<<10 {
		return nil, fmt.Errorf("%w: typed output exceeds 65536 bytes", ErrOutputContract)
	}
	summary := map[string]any{"output": output}
	if contract != nil {
		summary["contentType"] = firstNonEmpty(contract.ContentType, "application/json")
		summary["schemaVersion"] = contract.SchemaVersion
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func declaredStep(spec WorkflowSpec, name string) *StepSpec {
	for index := range spec.Steps {
		if spec.Steps[index].Name == name {
			return &spec.Steps[index]
		}
	}
	return nil
}

func decodeStoredSpec(raw json.RawMessage) (WorkflowSpec, error) {
	var spec WorkflowSpec
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&spec); err != nil {
		return spec, err
	}
	return spec, nil
}

// renderGoal appends the bounded dependency outputs to the step goal so
// Agent A's result reaches Agent B through AgentOS, never peer to peer.
func renderGoal(goal string, outputs map[string]string) string {
	if len(outputs) == 0 {
		return goal
	}
	rendered := goal
	for _, name := range sortedKeys(outputs) {
		rendered += fmt.Sprintf("\n\nUpstream result [%s]:\n%s", name, outputs[name])
	}
	return rendered
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func resultSummaryOutput(raw json.RawMessage) string {
	var summary map[string]json.RawMessage
	if json.Unmarshal(raw, &summary) == nil {
		output := summary["output"]
		var text string
		if json.Unmarshal(output, &text) == nil {
			return text
		}
		return string(output)
	}
	return ""
}

func resolveJSONPointer(document any, pointer string) (any, bool) {
	if pointer == "" {
		return document, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	current := document
	for _, rawToken := range strings.Split(pointer[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(rawToken, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[token]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func containsBounded(haystack, needle string) bool {
	if len(haystack) > 1<<20 {
		haystack = haystack[:1<<20]
	}
	return len(needle) > 0 && strings.Contains(haystack, needle)
}

func skipStepInput(workflow kernelstore.Workflow, step kernelstore.WorkflowStep, code string) kernelstore.TransitionWorkflowStepInput {
	return kernelstore.TransitionWorkflowStepInput{
		TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
		ExpectedVersion: step.ResourceVersion, To: kernelstore.StepSkipped, FailureCode: code,
	}
}
