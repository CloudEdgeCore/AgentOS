// Package workflow implements the v1.2 orchestrator: WorkflowRun and Step
// state, dependency dispatch with conditions and joins, single-step retry,
// human approval, cancellation propagation and restart recovery. The
// orchestrator decides who executes when and creates ordinary Tasks; it
// never schedules (that stays with the scheduler) and agents never talk to
// each other directly — Agent A's result reaches Agent B through AgentOS.
package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
)

const (
	maxSteps           = 1024
	maxGoalBytes       = 8192
	maxSpecBytes       = 1 << 20
	maxConditionBytes  = 1024
	maxDefaultAttempts = 1
	maxRetryAttempts   = 10
)

// StepCondition is the structured condition of one step: evaluated against
// the stored result summary of one dependency step. Exactly one predicate
// must be set; a false predicate skips the step (CONDITION_NOT_MET).
type StepCondition struct {
	Step           string `json:"step"`
	OutputContains string `json:"outputContains,omitempty"`
	OutputEquals   string `json:"outputEquals,omitempty"`
}

// StepRetry bounds the single-step retry budget (total task attempts).
type StepRetry struct {
	MaxAttempts int `json:"maxAttempts"`
}

// StepSpec is one declared DAG node of the workflow document.
type StepSpec struct {
	Name             string          `json:"name"`
	AgentVersionRef  string          `json:"agentVersionRef"`
	Goal             string          `json:"goal"`
	Spec             json.RawMessage `json:"spec,omitempty"`
	DependsOn        []string        `json:"dependsOn,omitempty"`
	Condition        *StepCondition  `json:"condition,omitempty"`
	Retry            *StepRetry      `json:"retry,omitempty"`
	RequiresApproval bool            `json:"requiresApproval"`
}

// WorkflowSpec is the user-facing workflow document.
type WorkflowSpec struct {
	DefaultTaskSpec json.RawMessage `json:"defaultTaskSpec,omitempty"`
	Steps           []StepSpec      `json:"steps"`
}

// DecodeSpec strictly decodes and fully validates one workflow document,
// returning the step inputs (with default/step spec overlays merged) for
// durable storage.
func DecodeSpec(raw []byte) ([]kernelstore.CreateWorkflowStepInput, error) {
	if len(raw) == 0 || len(raw) > maxSpecBytes {
		return nil, fmt.Errorf("workflow spec must be 1..%d bytes (a 1024-step DAG needs ~150KB)", maxSpecBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var spec WorkflowSpec
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("workflow spec: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("workflow spec contains more than one JSON value")
	}
	if len(spec.Steps) == 0 || len(spec.Steps) > maxSteps {
		return nil, fmt.Errorf("workflow must declare 1..%d steps", maxSteps)
	}
	if len(spec.DefaultTaskSpec) > 0 {
		if !json.Valid(spec.DefaultTaskSpec) {
			return nil, fmt.Errorf("defaultTaskSpec must be a JSON object")
		}
	}

	names := make(map[string]int, len(spec.Steps))
	for index, step := range spec.Steps {
		if err := kernelstore.ValidateStepName(step.Name); err != nil {
			return nil, fmt.Errorf("steps[%d]: %w", index, err)
		}
		if _, duplicate := names[step.Name]; duplicate {
			return nil, fmt.Errorf("duplicate step name %q", step.Name)
		}
		names[step.Name] = index
	}

	// Validate each node, then the graph.
	edges := make(map[string][]string, len(spec.Steps))
	defaults := objectMap(spec.DefaultTaskSpec)
	for index, step := range spec.Steps {
		if step.AgentVersionRef == "" || len(step.AgentVersionRef) > 256 {
			return nil, fmt.Errorf("steps[%d].agentVersionRef is required and bounded", index)
		}
		if step.Goal == "" || len(step.Goal) > maxGoalBytes {
			return nil, fmt.Errorf("steps[%d].goal must be 1..%d bytes", index, maxGoalBytes)
		}
		if len(step.Spec) > 0 && !json.Valid(step.Spec) {
			return nil, fmt.Errorf("steps[%d].spec must be a JSON object", index)
		}
		seenDeps := map[string]struct{}{}
		for _, dependency := range step.DependsOn {
			if _, ok := names[dependency]; !ok {
				return nil, fmt.Errorf("steps[%d] depends on unknown step %q", index, dependency)
			}
			if dependency == step.Name {
				return nil, fmt.Errorf("steps[%d] depends on itself", index)
			}
			if _, duplicate := seenDeps[dependency]; duplicate {
				return nil, fmt.Errorf("steps[%d] declares dependency %q twice", index, dependency)
			}
			seenDeps[dependency] = struct{}{}
		}
		edges[step.Name] = step.DependsOn

		if step.Retry != nil && (step.Retry.MaxAttempts < 1 || step.Retry.MaxAttempts > maxRetryAttempts) {
			return nil, fmt.Errorf("steps[%d].retry.maxAttempts must be 1..%d", index, maxRetryAttempts)
		}
		if step.Condition != nil {
			if len(step.DependsOn) == 0 {
				return nil, fmt.Errorf("steps[%d].condition requires dependsOn", index)
			}
			if _, ok := names[step.Condition.Step]; !ok {
				return nil, fmt.Errorf("steps[%d].condition.step %q is not a declared step", index, step.Condition.Step)
			}
			referenced := false
			for _, dependency := range step.DependsOn {
				if dependency == step.Condition.Step {
					referenced = true
				}
			}
			if !referenced {
				return nil, fmt.Errorf("steps[%d].condition.step must be one of dependsOn", index)
			}
			predicates := 0
			if step.Condition.OutputContains != "" {
				predicates++
				if len(step.Condition.OutputContains) > maxConditionBytes {
					return nil, fmt.Errorf("steps[%d].condition.outputContains exceeds %d bytes", index, maxConditionBytes)
				}
			}
			if step.Condition.OutputEquals != "" {
				predicates++
				if len(step.Condition.OutputEquals) > maxConditionBytes {
					return nil, fmt.Errorf("steps[%d].condition.outputEquals exceeds %d bytes", index, maxConditionBytes)
				}
			}
			if predicates != 1 {
				return nil, fmt.Errorf("steps[%d].condition must set exactly one predicate", index)
			}
		}
	}
	if cyclic := detectCycle(edges); cyclic {
		return nil, fmt.Errorf("workflow steps must form a directed acyclic graph")
	}

	inputs := make([]kernelstore.CreateWorkflowStepInput, 0, len(spec.Steps))
	for _, step := range spec.Steps {
		merged, err := mergeSpecs(defaults, objectMap(step.Spec))
		if err != nil {
			return nil, err
		}
		input := kernelstore.CreateWorkflowStepInput{
			Name: step.Name, Spec: merged, AgentVersionRef: step.AgentVersionRef, Goal: step.Goal,
			DependsOn: append([]string(nil), step.DependsOn...), RequiresApproval: step.RequiresApproval,
		}
		if step.Condition != nil {
			input.ConditionStep = step.Condition.Step
			input.ConditionContains = step.Condition.OutputContains
			input.ConditionEquals = step.Condition.OutputEquals
		}
		if step.Retry != nil {
			input.MaxAttempts = step.Retry.MaxAttempts
		} else {
			input.MaxAttempts = maxDefaultAttempts
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

// detectCycle runs reverse Kahn elimination over the dependency graph:
// dependents[x] counts the steps that still depend on x. Sinks (nothing
// depends on them) are removed first; removing a step releases its own
// dependencies. A graph that cannot be fully eliminated is cyclic.
func detectCycle(edges map[string][]string) bool {
	dependents := make(map[string]int, len(edges))
	for name := range edges {
		dependents[name] = 0
	}
	for _, dependencies := range edges {
		for _, dependency := range dependencies {
			dependents[dependency]++
		}
	}
	queue := make([]string, 0, len(dependents))
	for name, count := range dependents {
		if count == 0 {
			queue = append(queue, name)
		}
	}
	sort.Strings(queue)
	visited := 0
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		visited++
		for _, dependency := range edges[current] {
			dependents[dependency]--
			if dependents[dependency] == 0 {
				queue = append(queue, dependency)
			}
		}
	}
	return visited != len(edges)
}

func objectMap(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	return object
}

// mergeSpecs shallow-merges the workflow default task spec with the step
// overlay (step keys win) and returns canonical JSON.
func mergeSpecs(defaults, overlay map[string]json.RawMessage) (json.RawMessage, error) {
	if len(defaults) == 0 && len(overlay) == 0 {
		return nil, fmt.Errorf("a task spec is required (defaultTaskSpec or step spec)")
	}
	merged := make(map[string]json.RawMessage, len(defaults)+len(overlay))
	for key, value := range defaults {
		merged[key] = value
	}
	for key, value := range overlay {
		merged[key] = value
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode merged task spec: %w", err)
	}
	return encoded, nil
}
