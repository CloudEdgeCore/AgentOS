// Package policy evaluates default-deny Rego policies inside the kernel
// process. Policy decisions are enforced outside the LLM and outside per-call
// application code; the input document is constructed by trusted kernel code,
// and every failure mode (missing tenant policy, evaluation error, no rule
// match) denies by default.
package policy

import (
	"context"
	_ "embed"
	"fmt"
	"sort"

	"github.com/open-policy-agent/opa/v1/rego"
)

// Revision identifies the embedded policy module set. It is recorded with
// every decision so outcomes remain auditable across policy updates.
const Revision = "2026-08-15/v1"

//go:embed agentos.rego
var moduleSource string

// TenantPolicy is the tenant-attribute data the policy rules evaluate.
type TenantPolicy struct {
	MaxPriority int `json:"max_priority"`
}

// TenantPolicies maps tenant IDs to their policy data. A tenant without an
// entry is denied by default.
type TenantPolicies map[string]TenantPolicy

// TaskContext carries the bounded task attributes the v1alpha1 policy reads.
type TaskContext struct {
	Priority int `json:"priority"`
}

// Decision is the machine-readable policy outcome.
type Decision struct {
	Allow       bool
	DenyReasons []string
}

// Engine evaluates the embedded policy against tenant data. Construction
// compiles the module and fails closed at startup on any policy defect.
type Engine struct {
	prepared rego.PreparedEvalQuery
	tenants  TenantPolicies
}

func New(tenants TenantPolicies) (*Engine, error) {
	return newWithModule(moduleSource, tenants)
}

func newWithModule(source string, tenants TenantPolicies) (*Engine, error) {
	query, err := rego.New(
		rego.Query("data.agentos.policy"),
		rego.Module("agentos.rego", source),
	).PrepareForEval(context.Background())
	if err != nil {
		return nil, fmt.Errorf("prepare policy engine: %w", err)
	}
	return &Engine{prepared: query, tenants: tenants}, nil
}

func (e *Engine) Evaluate(ctx context.Context, tenantID string, task TaskContext) Decision {
	tenant, ok := e.tenants[tenantID]
	if !ok {
		return Decision{DenyReasons: []string{"TENANT_POLICY_NOT_FOUND"}}
	}
	resultSet, err := e.prepared.Eval(ctx, rego.EvalInput(map[string]any{
		"task":   map[string]any{"priority": task.Priority},
		"tenant": map[string]any{"max_priority": tenant.MaxPriority},
	}))
	if err != nil {
		return Decision{DenyReasons: []string{"POLICY_EVALUATION_FAILED"}}
	}
	if len(resultSet) == 0 || len(resultSet[0].Expressions) == 0 {
		return Decision{DenyReasons: []string{"DEFAULT_DENY"}}
	}
	document, ok := resultSet[0].Expressions[0].Value.(map[string]any)
	if !ok {
		return Decision{DenyReasons: []string{"DEFAULT_DENY"}}
	}
	allowed, _ := document["allow"].(bool)
	reasons := stringSet(document["deny_reasons"])
	if !allowed && len(reasons) == 0 {
		reasons = []string{"DEFAULT_DENY"}
	}
	return Decision{Allow: allowed, DenyReasons: reasons}
}

func stringSet(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var reasons []string
	for _, item := range items {
		if text, ok := item.(string); ok {
			reasons = append(reasons, text)
		}
	}
	sort.Strings(reasons)
	return reasons
}
