// Package admission evaluates deterministic pre-scheduling requirements.
package admission

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/bian-cloud-skill/agentos/internal/kernel/workload"
)

const EvaluatorVersion = "builtin/v1alpha1"

type Limits struct {
	RuntimeClasses    []string
	MaxTokens         int64
	MaxCostUSD        float64
	MaxToolCalls      int64
	MaxWallSeconds    int64
	MaxCPU            int64
	MaxMemory         int64
	MaxLLMConcurrency int
}

type Decision struct {
	Admit      bool
	ReasonCode string
	Reasons    []store.AdmissionReason
}

type Engine struct {
	limits Limits
	now    func() time.Time
}

func New(limits Limits) *Engine {
	return &Engine{limits: limits, now: func() time.Time { return time.Now().UTC() }}
}

func (e *Engine) Evaluate(task store.Task) Decision {
	spec, err := workload.Decode(task.Spec)
	if err != nil {
		return rejected("SPEC_INVALID", "spec", err.Error())
	}
	var reasons []store.AdmissionReason
	if spec.Priority < 0 || spec.Priority > 100 {
		reasons = append(reasons, reason("PRIORITY_OUT_OF_RANGE", "priority", "priority must be between 0 and 100"))
	}
	if spec.Deadline != nil && !spec.Deadline.After(e.now()) {
		reasons = append(reasons, reason("DEADLINE_EXPIRED", "deadline", "deadline must be in the future"))
	}
	if len(spec.Placement.RuntimeClasses) == 0 {
		reasons = append(reasons, reason("RUNTIME_CLASS_REQUIRED", "placement.runtimeClasses", "at least one runtime class is required"))
	}
	for _, runtimeClass := range spec.Placement.RuntimeClasses {
		if !slices.Contains(e.limits.RuntimeClasses, runtimeClass) {
			reasons = append(reasons, reason("RUNTIME_CLASS_DENIED", "placement.runtimeClasses", fmt.Sprintf("runtime class %q is not admitted", runtimeClass)))
		}
	}
	if strings.TrimSpace(spec.Placement.Region) == "" {
		reasons = append(reasons, reason("REGION_REQUIRED", "placement.region", "placement region is required"))
	}
	checkPositiveLimit(&reasons, "BUDGET_TOKENS_INVALID", "budget.tokens", spec.Budget.Tokens, e.limits.MaxTokens)
	checkPositiveLimit(&reasons, "BUDGET_TOOL_CALLS_INVALID", "budget.toolCalls", spec.Budget.ToolCalls, e.limits.MaxToolCalls)
	checkPositiveLimit(&reasons, "BUDGET_WALL_TIME_INVALID", "budget.wallSeconds", spec.Budget.WallSeconds, e.limits.MaxWallSeconds)
	if spec.Budget.CostUSD <= 0 || (e.limits.MaxCostUSD > 0 && spec.Budget.CostUSD > e.limits.MaxCostUSD) {
		reasons = append(reasons, reason("BUDGET_COST_INVALID", "budget.costUsd", "cost budget must be positive and within the tenant limit"))
	}
	checkPositiveLimit(&reasons, "CPU_REQUEST_INVALID", "placement.cpuMillis", spec.Placement.CPU, e.limits.MaxCPU)
	checkPositiveLimit(&reasons, "MEMORY_REQUEST_INVALID", "placement.memoryMiB", spec.Placement.Memory, e.limits.MaxMemory)
	if spec.Placement.LLMConcurrency <= 0 || (e.limits.MaxLLMConcurrency > 0 && spec.Placement.LLMConcurrency > e.limits.MaxLLMConcurrency) {
		reasons = append(reasons, reason("LLM_CONCURRENCY_INVALID", "placement.llmConcurrency", "LLM concurrency must be positive and within the tenant limit"))
	}
	if maxAttempts := spec.RetryPolicy.EffectiveMaxAttempts(); maxAttempts < 1 || maxAttempts > 10 {
		reasons = append(reasons, reason("RETRY_ATTEMPTS_INVALID", "retryPolicy.maxAttempts", "max attempts must be between 1 and 10"))
	}
	if len(reasons) != 0 {
		return Decision{ReasonCode: reasons[0].Code, Reasons: reasons}
	}
	return Decision{Admit: true, ReasonCode: "ADMISSION_PASSED", Reasons: []store.AdmissionReason{{Code: "ADMISSION_PASSED", Message: "all v1alpha1 admission checks passed"}}}
}

type Controller struct {
	store    store.ControlStore
	engine   *Engine
	ownerID  string
	batch    int
	claimTTL time.Duration
}

func NewController(repository store.ControlStore, engine *Engine, ownerID string, batch int, claimTTL time.Duration) *Controller {
	return &Controller{store: repository, engine: engine, ownerID: ownerID, batch: batch, claimTTL: claimTTL}
}

func (c *Controller) Reconcile(ctx context.Context) (int, error) {
	claims, err := c.store.ClaimTasks(ctx, store.ClaimTasksInput{
		Kind: store.ControllerAdmission, Phase: "QUEUED", OwnerID: c.ownerID, Limit: c.batch, TTL: c.claimTTL,
	})
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, claim := range claims {
		decision := c.engine.Evaluate(claim.Task)
		_, err := c.store.DecideAdmission(ctx, store.DecideAdmissionInput{
			TaskID: claim.Task.ID, TenantID: claim.Task.TenantID, OwnerID: claim.OwnerID,
			ClaimFencingToken: claim.FencingToken, ExpectedTaskVersion: claim.Task.ResourceVersion,
			Admit: decision.Admit, ReasonCode: decision.ReasonCode, Reasons: decision.Reasons,
			EvaluatorVersion: EvaluatorVersion,
		})
		if err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

func rejected(code, field, message string) Decision {
	return Decision{ReasonCode: code, Reasons: []store.AdmissionReason{reason(code, field, message)}}
}

func reason(code, field, message string) store.AdmissionReason {
	return store.AdmissionReason{Code: code, Field: field, Message: message}
}

func checkPositiveLimit(reasons *[]store.AdmissionReason, code, field string, value, limit int64) {
	if value <= 0 || (limit > 0 && value > limit) {
		*reasons = append(*reasons, reason(code, field, "value must be positive and within the tenant limit"))
	}
}
