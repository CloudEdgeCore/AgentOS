package agentpkg

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func testManifest() Manifest {
	return Manifest{
		Schema:          ManifestSchema,
		AgentVersionRef: "agent@1",
		SpecDigest:      SpecSHA256([]byte(`{"runtimeClassPolicy":{"allowed":["oci"]}}`)),
		Spec:            json.RawMessage(`{"runtimeClassPolicy":{"allowed":["oci"]}}`),
		RuntimeLock:     []Digest{{Algorithm: "sha256", Hex: "ab"}},
		Provenance: Provenance{
			Builder: "ci", BuildWorkflow: "build.yml", GitCommit: "abc123",
			BuiltAt: time.Unix(1700000000, 0).UTC(),
		},
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	signingKey, key, err := GenerateSigningKey("ci-builder-1")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	manifest := testManifest()
	pkg, err := Sign(manifest, signingKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	keys := map[string]ed25519.PublicKey{key.ID: key.PublicKey}
	if err := Verify(pkg, keys); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyRejectsTamperedManifest(t *testing.T) {
	signingKey, key, err := GenerateSigningKey("ci-builder-1")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pkg, err := Sign(testManifest(), signingKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Tamper with the spec while keeping the digest stale: admission must
	// reject before even looking at the signature.
	pkg.Manifest.Spec = json.RawMessage(`{"runtimeClassPolicy":{"allowed":["wasm"]}}`)
	if err := Verify(pkg, map[string]ed25519.PublicKey{key.ID: key.PublicKey}); err == nil {
		t.Fatal("tampered manifest must fail verification")
	}
}

func TestVerifyRejectsUnknownKeyAndBadSignature(t *testing.T) {
	signingKey, _, err := GenerateSigningKey("ci-builder-1")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	_, otherKey, err := GenerateSigningKey("attacker")
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	pkg, err := Sign(testManifest(), signingKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Unknown key id.
	if err := Verify(pkg, map[string]ed25519.PublicKey{otherKey.ID: otherKey.PublicKey}); !errors.Is(err, ErrPackageSignatureInvalid) {
		t.Fatalf("unknown key error = %v, want ErrPackageSignatureInvalid", err)
	}

	// Corrupt the signature bytes.
	pkg.Signature.Ed25519 = "AAAA"
	if err := Verify(pkg, map[string]ed25519.PublicKey{otherKey.ID: otherKey.PublicKey}); err == nil {
		t.Fatal("corrupt signature must fail")
	}
}

func TestVerifyRejectsUnsignedAndMalformed(t *testing.T) {
	// Nil package is unsigned.
	if err := Verify(nil, nil); !errors.Is(err, ErrPackageUnsigned) {
		t.Fatalf("nil package error = %v, want ErrPackageUnsigned", err)
	}

	// Valid manifest but no signing key: unsigned.
	if _, err := Sign(testManifest(), nil); !errors.Is(err, ErrPackageUnsigned) {
		t.Fatalf("unsigned sign error = %v, want ErrPackageUnsigned", err)
	}

	// Malformed manifest fails validation.
	bad := Manifest{Schema: "wrong", AgentVersionRef: "agent@1", Spec: json.RawMessage(`{}`), SpecDigest: SpecSHA256([]byte(`{}`))}
	if err := bad.Validate(); !errors.Is(err, ErrPackageManifestInvalid) {
		t.Fatalf("bad schema error = %v, want ErrPackageManifestInvalid", err)
	}
	pkg := &Package{Manifest: bad}
	if err := Verify(pkg, nil); !errors.Is(err, ErrPackageManifestInvalid) {
		t.Fatalf("bad manifest verify error = %v, want ErrPackageManifestInvalid", err)
	}

	// Spec digest mismatch.
	stale := testManifest()
	stale.Spec = json.RawMessage(`{"other":true}`)
	if err := stale.Validate(); !errors.Is(err, ErrPackageManifestInvalid) {
		t.Fatalf("stale digest error = %v, want ErrPackageManifestInvalid", err)
	}
}

func TestManifestDigestIsStable(t *testing.T) {
	first, err := ManifestDigest(testManifest())
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	second, err := ManifestDigest(testManifest())
	if err != nil {
		t.Fatalf("digest replay: %v", err)
	}
	if first != second {
		t.Fatal("manifest digest is not deterministic")
	}
}

func TestDigestVerify(t *testing.T) {
	content := []byte(`{"hello":"world"}`)
	d := SpecSHA256(content)
	if !d.Verify(content) {
		t.Fatal("digest must match its content")
	}
	if d.Verify([]byte(`tampered`)) {
		t.Fatal("digest must reject tampered content")
	}
	if (Digest{Algorithm: "md5", Hex: "00"}).Verify(content) {
		t.Fatal("non-sha256 digest must be rejected")
	}
}
