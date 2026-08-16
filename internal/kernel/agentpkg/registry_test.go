package agentpkg

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

func TestRegistryVerifyFailClosed(t *testing.T) {
	signingKey, key, err := GenerateSigningKey("ci-builder-1")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	registry := NewRegistry()
	if err := registry.Add(*key); err != nil {
		t.Fatalf("trust key: %v", err)
	}
	if registry.Len() != 1 {
		t.Fatalf("registry length = %d, want 1", registry.Len())
	}
	// Re-adding the same identity with the same key is a no-op.
	if err := registry.Add(*key); err != nil {
		t.Fatalf("re-add same key: %v", err)
	}
	if registry.Len() != 1 {
		t.Fatalf("registry length after re-add = %d, want 1", registry.Len())
	}
	// Re-adding the same identity with a different key is rejected.
	_, other, err := GenerateSigningKey("ci-builder-1")
	if err != nil {
		t.Fatalf("generate conflicting key: %v", err)
	}
	if err := registry.Add(*other); err == nil {
		t.Fatal("re-adding an identity with a different key must fail")
	}

	pkg, err := Sign(testManifest(), signingKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := registry.Verify(pkg); err != nil {
		t.Fatalf("verify trusted package: %v", err)
	}
	if err := registry.Verify(nil); !errors.Is(err, ErrPackageUnsigned) {
		t.Fatalf("verify nil error = %v, want ErrPackageUnsigned", err)
	}

	// An unsigned-looking envelope with a bogus key is rejected.
	bogus := &Package{Manifest: testManifest(), Signature: Signature{KeyID: "nobody", Ed25519: "AAAA"}}
	if err := registry.Verify(bogus); !errors.Is(err, ErrPackageSignatureInvalid) {
		t.Fatalf("verify unknown key error = %v, want ErrPackageSignatureInvalid", err)
	}
}

func TestKeyEncodingRoundTrip(t *testing.T) {
	_, key, err := GenerateSigningKey("ci")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	encoded := EncodePublicKey(key.PublicKey)
	decoded, err := DecodePublicKey(encoded)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if !equalPublicKey(decoded, key.PublicKey) {
		t.Fatal("decoded public key differs")
	}
	if _, err := DecodePublicKey("not-base64!!"); err == nil {
		t.Fatal("garbage public key must fail decoding")
	}
}

func TestRegistryKeyShapeValidation(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Add(Key{ID: "", PublicKey: make(ed25519.PublicKey, ed25519.PublicKeySize)}); err == nil {
		t.Fatal("empty key id must be rejected")
	}
	if err := registry.Add(Key{ID: "x", PublicKey: make(ed25519.PublicKey, 8)}); err == nil {
		t.Fatal("short public key must be rejected")
	}
}
