package workflow

import (
	"encoding/json"
	"testing"
)

// TestMergeSpecsPlacementDeepMerge verifies that a placement overlay merges
// field-wise over the workflow default placement, so an overlay can narrow
// runtimeClasses/preferredClass while inheriting the rest.
func TestMergeSpecsPlacementDeepMerge(t *testing.T) {
	base := map[string]json.RawMessage{
		"placement": json.RawMessage(`{
			"runtimeClasses": ["research-reasoning","research-network","research-sandbox"],
			"preferredClass": "research-reasoning",
			"region": "cn-east",
			"cpuMillis": 250,
			"memoryMiB": 256,
			"workspaceBytes": 8388608,
			"llmConcurrency": 2
		}`),
		"budget": json.RawMessage(`{"tokens": 40000, "costUsd": 5, "toolCalls": 40}`),
	}
	overlay := map[string]json.RawMessage{
		"placement": json.RawMessage(`{
			"runtimeClasses": ["research-sandbox"],
			"preferredClass": "research-sandbox"
		}`),
	}
	merged, err := mergeSpecs(base, overlay)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var spec struct {
		Placement map[string]json.RawMessage `json:"placement"`
		Budget    map[string]json.RawMessage `json:"budget"`
	}
	if err := json.Unmarshal(merged, &spec); err != nil {
		t.Fatalf("decode merged: %v", err)
	}
	if spec.Placement == nil {
		t.Fatalf("merged spec lost the placement object: %s", merged)
	}
	if got := string(spec.Placement["runtimeClasses"]); got != `["research-sandbox"]` {
		t.Fatalf("runtimeClasses = %s, want sandbox-only", got)
	}
	if got := string(spec.Placement["preferredClass"]); got != `"research-sandbox"` {
		t.Fatalf("preferredClass = %s, want research-sandbox", got)
	}
	// Inherited fields survive the overlay.
	for field, want := range map[string]string{
		"region": `"cn-east"`, "cpuMillis": `250`, "memoryMiB": `256`,
		"workspaceBytes": `8388608`, "llmConcurrency": `2`,
	} {
		if got := string(spec.Placement[field]); got != want {
			t.Fatalf("placement.%s = %s, want %s (inherited from defaults)", field, got, want)
		}
	}
	// Non-placement keys are untouched.
	if got := string(spec.Budget["tokens"]); got != `40000` {
		t.Fatalf("budget.tokens = %s, want 40000", got)
	}
}

// TestMergeSpecsPlacementOverlayOnly verifies a placement-only overlay over
// defaults without a placement still yields the overlay placement.
func TestMergeSpecsPlacementOverlayOnly(t *testing.T) {
	base := map[string]json.RawMessage{"budget": json.RawMessage(`{"tokens": 1000}`)}
	overlay := map[string]json.RawMessage{
		"placement": json.RawMessage(`{"runtimeClasses": ["research-sandbox"]}`),
	}
	merged, err := mergeSpecs(base, overlay)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !json.Valid(merged) {
		t.Fatalf("merged spec is not valid JSON: %s", merged)
	}
	var spec struct {
		Placement map[string]json.RawMessage `json:"placement"`
	}
	if err := json.Unmarshal(merged, &spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(spec.Placement["runtimeClasses"]); got != `["research-sandbox"]` {
		t.Fatalf("runtimeClasses = %s", got)
	}
}

// TestMergeSpecsPlacementNullOverlay ensures a non-object placement overlay
// is not silently merged (admission rejects it downstream).
func TestMergeSpecsPlacementNullOverlay(t *testing.T) {
	base := map[string]json.RawMessage{
		"placement": json.RawMessage(`{"runtimeClasses": ["adapter"], "region": "cn-east", "cpuMillis": 1, "memoryMiB": 1, "llmConcurrency": 1}`),
	}
	overlay := map[string]json.RawMessage{"placement": json.RawMessage(`null`)}
	merged, err := mergeSpecs(base, overlay)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var spec struct {
		Placement json.RawMessage `json:"placement"`
	}
	if err := json.Unmarshal(merged, &spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(spec.Placement) != "null" {
		t.Fatalf("placement = %s, want null passthrough", spec.Placement)
	}
}
