// Package errorcode is the canonical registry for stable public AgentOS error
// codes. Internal errors must be translated to one of these codes before they
// cross an Agent, SDK, API, artifact, event, or audit boundary.
package errorcode

import "sort"

const (
	Internal                 = "INTERNAL_ERROR"
	InvalidTask              = "INVALID_TASK"
	InvalidWorkflow          = "INVALID_WORKFLOW"
	ResourceVersionConflict  = "RESOURCE_VERSION_CONFLICT"
	IdempotencyConflict      = "IDEMPOTENCY_CONFLICT"
	AuthenticationRequired   = "AUTHENTICATION_REQUIRED"
	PolicyDenied             = "POLICY_DENIED"
	TenantQuotaExceeded      = "TENANT_QUOTA_EXCEEDED"
	BudgetExhausted          = "BUDGET_EXHAUSTED"
	WallTimeExceeded         = "WALL_TIME_EXCEEDED"
	ProviderUnavailable      = "PROVIDER_UNAVAILABLE"
	ProviderRejected         = "PROVIDER_REJECTED"
	ProviderStreamAborted    = "PROVIDER_STREAM_ABORTED"
	ToolInvocationFailed     = "TOOL_INVOCATION_FAILED"
	OutputContractViolation  = "OUTPUT_CONTRACT_VIOLATION"
	WorkflowDeadlineExceeded = "WORKFLOW_DEADLINE_EXCEEDED"
	WorkflowBudgetExhausted  = "WORKFLOW_BUDGET_EXHAUSTED"
	SpawnDisabled            = "SPAWN_DISABLED"
	SpawnCancelled           = "SPAWN_CANCELLED"
	SpawnDeadlineExceeded    = "SPAWN_DEADLINE_EXCEEDED"
	SpawnBudgetExhausted     = "SPAWN_BUDGET_EXHAUSTED"
	SpawnDepthExceeded       = "SPAWN_DEPTH_EXCEEDED"
	SpawnFanoutExceeded      = "SPAWN_FANOUT_EXCEEDED"
	SpawnDynamicExceeded     = "SPAWN_DYNAMIC_LIMIT_EXCEEDED"
	SpawnTotalStepsExceeded  = "SPAWN_TOTAL_STEPS_EXCEEDED"
	SpawnTaskLimitExceeded   = "SPAWN_TASK_LIMIT_EXCEEDED"
	SpawnTokenLimitExceeded  = "SPAWN_TOKEN_LIMIT_EXCEEDED"
	SpawnCostLimitExceeded   = "SPAWN_COST_LIMIT_EXCEEDED"
	SpawnNameConflict        = "SPAWN_NAME_CONFLICT"
)

var known = map[string]struct{}{
	Internal: {}, InvalidTask: {}, InvalidWorkflow: {}, ResourceVersionConflict: {}, IdempotencyConflict: {},
	AuthenticationRequired: {}, PolicyDenied: {}, TenantQuotaExceeded: {}, BudgetExhausted: {}, WallTimeExceeded: {},
	ProviderUnavailable: {}, ProviderRejected: {}, ProviderStreamAborted: {}, ToolInvocationFailed: {},
	OutputContractViolation: {}, WorkflowDeadlineExceeded: {}, WorkflowBudgetExhausted: {}, SpawnDisabled: {},
	SpawnCancelled: {}, SpawnDeadlineExceeded: {}, SpawnBudgetExhausted: {}, SpawnDepthExceeded: {},
	SpawnFanoutExceeded: {}, SpawnDynamicExceeded: {}, SpawnTotalStepsExceeded: {}, SpawnTaskLimitExceeded: {},
	SpawnTokenLimitExceeded: {}, SpawnCostLimitExceeded: {}, SpawnNameConflict: {},
}

func Known(code string) bool {
	_, ok := known[code]
	return ok
}

func All() []string {
	codes := make([]string, 0, len(known))
	for code := range known {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}
