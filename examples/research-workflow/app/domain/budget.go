package domain

import "fmt"

// Role budget reference of design doc §10. The workflow template's
// defaultTaskSpec stays the operational truth for kernel admission (the
// kernel reserves each declared step's task ceiling against the workflow
// budget), so these values document the per-role intent and drive the
// budget-calculation unit tests of the test matrix (§16).
var RoleBudgetTokens = map[string]int64{
	"planner":            20000,
	"search":             5000,
	"reader":             6000,
	"analyst":            30000,
	"critic":             20000,
	"writer":             30000,
	"citation-validator": 15000,
}

// Hard limits of design doc §10. PlannerMaxQuestions is the application
// decoder bound (the runtime truncates at 6, inside this ceiling).
const (
	PlannerMaxQuestions         = 8
	SearchMaxSourcesPerQuesiton = 8
	ReaderMaxSourceBytes        = 2 << 20 // 2 MiB
	CriticMaxRounds             = 3
	WorkflowMaxTasks            = 80
)

// WorkflowBudgetDefaults are the §10 workflow-level defaults. Note the
// kernel's budget-commitment rule (every declared step reserves its task
// ceiling) makes the illustrative 250k token default unreachable for the
// 11-step research template; the API server therefore validates request
// overrides against the kernel decoder and fails closed with the computed
// floor instead of silently raising the number.
type WorkflowBudgetDefaults struct {
	MaxTasks        int64
	MaxTokens       int64
	MaxToolCalls    int64
	MaxCostMicroUSD int64
}

// DefaultWorkflowBudget returns the §10 defaults.
func DefaultWorkflowBudget() WorkflowBudgetDefaults {
	return WorkflowBudgetDefaults{
		MaxTasks:        80,
		MaxTokens:       250000,
		MaxToolCalls:    250,
		MaxCostMicroUSD: 5000000,
	}
}

// SumRoleBudgetTokens adds the per-role budgets (§16 "Research Budget
// Calculation" unit subject).
func SumRoleBudgetTokens() int64 {
	total := int64(0)
	for _, tokens := range RoleBudgetTokens {
		total += tokens
	}
	return total
}

// ResearchDecompositionBudget returns the token ceiling the planner may
// spend for one research run per §10.
func ResearchDecompositionBudget() int64 {
	return RoleBudgetTokens["planner"]
}

// ValidateRequestBudget range-checks the §13 request budget override before
// it is applied to the workflow document. Zero leaves the template default.
func ValidateRequestBudget(maxTasks, maxTokens int64) error {
	if maxTasks < 0 {
		return fmt.Errorf("budget.maxTasks must be >= 0 (0 keeps the template default)")
	}
	if maxTasks > WorkflowMaxTasks {
		return fmt.Errorf("budget.maxTasks %d exceeds the research hard limit of %d", maxTasks, WorkflowMaxTasks)
	}
	if maxTokens < 0 {
		return fmt.Errorf("budget.maxTokens must be >= 0 (0 keeps the template default)")
	}
	return nil
}
