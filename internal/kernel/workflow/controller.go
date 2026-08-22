// The v1.2 orchestrator controller. Reconcile is stateless and durable: all
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
	"strings"
	"sync"
	"time"

	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
)

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
	now         func() time.Time
	newID       func() uuid.UUID
	logger      *slog.Logger
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

// defaultMaxInFlightSteps caps dispatched-but-unfinished steps per workflow.
const defaultMaxInFlightSteps = 64

// WithMaxInFlightSteps bounds concurrently dispatched steps per workflow.
func (c *Controller) WithMaxInFlightSteps(limit int) *Controller {
	if limit > 0 {
		c.maxInFlight = limit
	}
	return c
}

// Reconcile drives every active workflow one round: dispatching ready
// steps, observing task terminals, applying retries, propagating
// cancellation and finalizing workflows. Retryable transaction conflicts
// are retried with bounded backoff (ADR-002).
func (c *Controller) Reconcile(ctx context.Context) (int, error) {
	active, err := c.workflows.ListActiveWorkflows(ctx, c.batch)
	if err != nil {
		return 0, err
	}
	if len(active) == 0 {
		return 0, nil
	}
	processed := 0
	if c.parallel <= 1 || len(active) == 1 {
		for _, workflow := range active {
			ok, err := c.processWorkflow(ctx, workflow)
			if err != nil {
				return processed, err
			}
			if ok {
				processed++
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
			ok, err := c.processWorkflow(ctx, workflow)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if batchErr == nil {
					batchErr = err
				}
				return
			}
			if ok {
				processed++
			}
		}()
	}
	wg.Wait()
	return processed, batchErr
}

// processWorkflow advances one workflow; it reports whether state moved.
func (c *Controller) processWorkflow(ctx context.Context, workflow kernelstore.Workflow) (bool, error) {
	steps, err := c.workflows.ListWorkflowSteps(ctx, workflow.TenantID, workflow.ID)
	if err != nil {
		return false, err
	}
	byName := make(map[string]kernelstore.WorkflowStep, len(steps))
	for _, step := range steps {
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

	inFlight := 0
	for _, step := range steps {
		if step.Status == kernelstore.StepRunning {
			inFlight++
		}
	}
	for _, step := range steps {
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
		if declared.RequiresApproval && step.DecidedBy == approvalRejected {
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
	if declared == nil {
		return false, fmt.Errorf("step %q missing from stored spec", step.Name)
	}
	// Dependency gating (join semantics: every dependency must succeed).
	dependencyOutputs := make(map[string]string, len(declared.DependsOn))
	for _, dependency := range declared.DependsOn {
		upstream := byName[dependency]
		switch upstream.Status {
		case kernelstore.StepSucceeded:
			dependencyOutputs[dependency] = resultSummaryOutput(upstream.ResultSummary)
		case kernelstore.StepFailed, kernelstore.StepSkipped, kernelstore.StepCancelled:
			// A failed upstream never executes its dependents.
			_, err := c.workflows.TransitionWorkflowStep(ctx, skipStepInput(workflow, step, "UPSTREAM_NOT_SUCCEEDED"))
			return true, err
		default:
			return false, nil // still waiting on the dependency
		}
	}
	// Condition evaluation against the stored dependency output.
	if declared.Condition != nil {
		output := dependencyOutputs[declared.Condition.Step]
		met := false
		switch {
		case declared.Condition.OutputContains != "":
			met = containsBounded(output, declared.Condition.OutputContains)
		case declared.Condition.OutputEquals != "":
			met = output == declared.Condition.OutputEquals
		}
		if !met {
			_, err := c.workflows.TransitionWorkflowStep(ctx, skipStepInput(workflow, step, "CONDITION_NOT_MET"))
			return true, err
		}
	}
	if declared.RequiresApproval && step.DecidedBy == "" && step.Status == kernelstore.StepPending {
		_, err := c.workflows.TransitionWorkflowStep(ctx, kernelstore.TransitionWorkflowStepInput{
			TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
			ExpectedVersion: step.ResourceVersion, To: kernelstore.StepWaitingApproval,
		})
		return true, err
	}

	// The stored spec is the raw document; re-apply the default/overlay
	// merge exactly as publication validation did.
	mergedSpec, err := mergeSpecs(objectMap(spec.DefaultTaskSpec), objectMap(declared.Spec))
	if err != nil {
		return false, fmt.Errorf("step %q task spec: %w", step.Name, err)
	}

	// Idempotent per (workflow, step, attempt): racing orchestrators create
	// exactly one Task.
	attempt := step.AttemptCount + 1
	created, err := c.tasks.CreateTask(ctx, kernelstore.CreateTaskInput{
		ID: c.newID(), TenantID: workflow.TenantID, Namespace: workflow.Namespace,
		AgentVersionRef: declared.AgentVersionRef, Goal: renderGoal(declared.Goal, dependencyOutputs),
		Spec: mergedSpec, IdempotencyKey: fmt.Sprintf("workflow/%s/%s/%d", workflow.ID, step.Name, attempt),
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
		summary, extractErr := c.extractResult(ctx, workflow.TenantID, task)
		if extractErr != nil {
			c.logger.Warn("workflow step result extraction failed", "step", step.Name, "error", extractErr)
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
		declared := declaredStep(spec, step.Name)
		maxAttempts := 1
		if declared != nil && declared.Retry != nil {
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
func (c *Controller) extractResult(ctx context.Context, tenantID string, task kernelstore.Task) (json.RawMessage, error) {
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
	encoded, err := json.Marshal(map[string]any{"output": boundedText(output, 4096)})
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
	var summary struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(raw, &summary) == nil {
		return summary.Output
	}
	return ""
}

func containsBounded(haystack, needle string) bool {
	if len(haystack) > 1<<20 {
		haystack = haystack[:1<<20]
	}
	return len(needle) > 0 && strings.Contains(haystack, needle)
}

func boundedText(value any, limit int) string {
	text := ""
	switch typed := value.(type) {
	case string:
		text = typed
	default:
		encoded, err := json.Marshal(value)
		if err == nil {
			text = string(encoded)
		}
	}
	if len(text) > limit {
		return text[:limit]
	}
	return text
}

func skipStepInput(workflow kernelstore.Workflow, step kernelstore.WorkflowStep, code string) kernelstore.TransitionWorkflowStepInput {
	return kernelstore.TransitionWorkflowStepInput{
		TenantID: workflow.TenantID, WorkflowID: workflow.ID, StepName: step.Name,
		ExpectedVersion: step.ResourceVersion, To: kernelstore.StepSkipped, FailureCode: code,
	}
}
