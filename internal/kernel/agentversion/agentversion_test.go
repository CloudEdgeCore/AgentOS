package agentversion

import (
	"encoding/json"
	"testing"
)

func TestParseRefAcceptsCanonicalReferences(t *testing.T) {
	for _, ref := range []string{"research-agent@1.3.0", "agent@1", "a.b-c_d@v0.1.2"} {
		name, version, err := ParseRef(ref)
		if err != nil {
			t.Fatalf("ParseRef(%q) error: %v", ref, err)
		}
		if name == "" || version == "" {
			t.Fatalf("ParseRef(%q) = (%q, %q), want non-empty halves", ref, name, version)
		}
	}
}

func TestParseRefRejectsMalformedReferences(t *testing.T) {
	for _, ref := range []string{
		"", "agent", "agent@", "@1", "agent@1@2", "agent v1", "agent:v1",
		"@", "a@b c", "-agent@1", "agent@-1", "agent@1/2",
	} {
		if _, _, err := ParseRef(ref); err == nil {
			t.Fatalf("ParseRef(%q) unexpectedly succeeded", ref)
		}
	}
}

func TestCanonicalizeSpecIsDeterministicAndObjectBounded(t *testing.T) {
	first := []byte(`{"lifecycle":{"maxAttempts":3},"runtimeClassPolicy":{"allowed":["oci","wasm"]}}`)
	reordered := []byte(`{"runtimeClassPolicy":{"allowed":["oci","wasm"]},"lifecycle":{"maxAttempts":3}}`)
	firstCanonical, firstDigest, err := CanonicalizeSpec(first)
	if err != nil {
		t.Fatalf("canonicalize first: %v", err)
	}
	reorderedCanonical, reorderedDigest, err := CanonicalizeSpec(reordered)
	if err != nil {
		t.Fatalf("canonicalize reordered: %v", err)
	}
	if firstDigest != reorderedDigest || string(firstCanonical) != string(reorderedCanonical) {
		t.Fatalf("canonicalization is not deterministic: %s vs %s", firstCanonical, reorderedCanonical)
	}
	if _, _, err := CanonicalizeSpec([]byte(`[1,2,3]`)); err == nil {
		t.Fatal("array spec was accepted")
	}
	if _, _, err := CanonicalizeSpec([]byte(`null`)); err == nil {
		t.Fatal("null spec was accepted")
	}
}

func TestCanonicalizeSpecPreservesIntegerNumbers(t *testing.T) {
	canonical, _, err := CanonicalizeSpec(json.RawMessage(`{"budget":{"tokens":200000}}`))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if string(canonical) != `{"budget":{"tokens":200000}}` {
		t.Fatalf("integer was rewritten: %s", canonical)
	}
}

func TestValidateSpecBoundsTheKernelSubset(t *testing.T) {
	valid := []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"runtimeClassPolicy":{"allowed":["oci","wasm"],"preferred":"oci"}}`),
		json.RawMessage(`{"lifecycle":{"maxAttempts":5}}`),
		json.RawMessage(`{"runtimeClassPolicy":{"allowed":["oci"]},"modelPolicy":{"route":"quality"}}`),
	}
	for _, spec := range valid {
		if err := ValidateSpec(spec); err != nil {
			t.Fatalf("ValidateSpec(%s) error: %v", spec, err)
		}
	}
	invalid := []json.RawMessage{
		json.RawMessage(`{"runtimeClassPolicy":{"allowed":[""],"preferred":"oci"}}`),
		json.RawMessage(`{"runtimeClassPolicy":{"allowed":["oci","oci"]}}`),
		json.RawMessage(`{"runtimeClassPolicy":{"preferred":"oci"}}`),
		json.RawMessage(`{"runtimeClassPolicy":{"allowed":["oci"],"preferred":"microvm"}}`),
		json.RawMessage(`{"lifecycle":{"maxAttempts":11}}`),
		json.RawMessage(`{"lifecycle":{"maxAttempts":-1}}`),
		json.RawMessage(`[1]`),
	}
	for _, spec := range invalid {
		if err := ValidateSpec(spec); err == nil {
			t.Fatalf("ValidateSpec(%s) unexpectedly succeeded", spec)
		}
	}
}

func TestValidateNameAndVersion(t *testing.T) {
	for _, token := range []string{"research-agent", "agent", "a.b_c-1", "V1"} {
		if err := ValidateName(token); err != nil {
			t.Fatalf("ValidateName(%q) error: %v", token, err)
		}
		if err := ValidateVersion(token); err != nil {
			t.Fatalf("ValidateVersion(%q) error: %v", token, err)
		}
	}
	for _, token := range []string{"", "-agent", "agent:", "agent/v1", "agent v1", "agent@1"} {
		if err := ValidateName(token); err == nil {
			t.Fatalf("ValidateName(%q) unexpectedly succeeded", token)
		}
		if err := ValidateVersion(token); err == nil {
			t.Fatalf("ValidateVersion(%q) unexpectedly succeeded", token)
		}
	}
}
