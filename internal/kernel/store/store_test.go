package store

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestCreateTaskInputHashNormalizesObjectKeyOrder(t *testing.T) {
	t.Parallel()

	a := CreateTaskInput{
		TenantID: "tenant", Namespace: "default", AgentVersionRef: "agent@1", Goal: "test",
		Spec: json.RawMessage(`{"a":1,"b":{"x":true}}`), IdempotencyKey: "key",
	}
	b := a
	b.Spec = json.RawMessage(` { "b": { "x": true }, "a": 1 } `)

	normalizedA, hashA, err := a.ValidateAndHash()
	if err != nil {
		t.Fatalf("ValidateAndHash(a): %v", err)
	}
	normalizedB, hashB, err := b.ValidateAndHash()
	if err != nil {
		t.Fatalf("ValidateAndHash(b): %v", err)
	}
	if string(normalizedA) != string(normalizedB) || hashA != hashB {
		t.Fatalf("equivalent JSON produced different identity: %s / %s", normalizedA, normalizedB)
	}
}

func TestCreateTaskInputRejectsInvalidOrTrailingJSON(t *testing.T) {
	t.Parallel()

	base := CreateTaskInput{
		TenantID: "tenant", Namespace: "default", AgentVersionRef: "agent@1", Goal: "test",
		IdempotencyKey: "key",
	}
	for _, spec := range []string{"", "{", "{} {}"} {
		input := base
		input.Spec = json.RawMessage(spec)
		if _, _, err := input.ValidateAndHash(); err == nil {
			t.Fatalf("spec %q unexpectedly passed", spec)
		}
	}
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	t.Parallel()

	if errors.Is(ErrFenced, ErrVersionConflict) || errors.Is(ErrLeaseHeld, ErrFenced) {
		t.Fatal("store sentinel errors must remain independently classifiable")
	}
}
