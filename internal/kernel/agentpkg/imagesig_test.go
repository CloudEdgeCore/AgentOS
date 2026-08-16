package agentpkg

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const testImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestSignImageRoundTrip(t *testing.T) {
	signingKey, key, err := GenerateSigningKey("ci-builder-1")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	manifest := testManifest()
	manifest.SignedImageDigest = Digest{Algorithm: "sha256", Hex: strings.TrimPrefix(testImageDigest, "sha256:")}
	signature, err := SignImage(manifest.SignedImageDigest, signingKey)
	if err != nil {
		t.Fatalf("sign image: %v", err)
	}
	manifest.ImageSignature = signature
	pkg, err := Sign(manifest, signingKey)
	if err != nil {
		t.Fatalf("sign package: %v", err)
	}
	keys := map[string]ed25519.PublicKey{key.ID: key.PublicKey}
	if err := Verify(pkg, keys); err != nil {
		t.Fatalf("verify with image signature: %v", err)
	}
}

func TestVerifyImageSignatureFailClosed(t *testing.T) {
	signingKey, key, err := GenerateSigningKey("ci-builder-1")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	manifest := testManifest()
	manifest.SignedImageDigest = Digest{Algorithm: "sha256", Hex: strings.TrimPrefix(testImageDigest, "sha256:")}
	signature, err := SignImage(manifest.SignedImageDigest, signingKey)
	if err != nil {
		t.Fatalf("sign image: %v", err)
	}
	manifest.ImageSignature = signature
	keys := map[string]ed25519.PublicKey{key.ID: key.PublicKey}

	// A different image digest breaks the image signature.
	tampered := manifest
	tampered.SignedImageDigest = Digest{Algorithm: "sha256", Hex: strings.Repeat("b", 64)}
	if err := VerifyImageSignature(tampered, keys); !errors.Is(err, ErrPackageSignatureInvalid) {
		t.Fatalf("tampered image digest error = %v, want ErrPackageSignatureInvalid", err)
	}

	// An unknown key breaks the image signature.
	unknown := manifest
	unknown.ImageSignature = &Signature{KeyID: "attacker", Ed25519: signature.Ed25519}
	if err := VerifyImageSignature(unknown, keys); !errors.Is(err, ErrPackageSignatureInvalid) {
		t.Fatalf("unknown image key error = %v, want ErrPackageSignatureInvalid", err)
	}

	// A signature over the manifest digest must not verify as an image
	// signature (namespaced payloads).
	manifestDigest, err := ManifestDigest(manifest)
	if err != nil {
		t.Fatalf("manifest digest: %v", err)
	}
	manifestSig := ed25519.Sign(signingKey.PrivateKey, manifestDigest[:])
	forged := manifest
	forged.ImageSignature = &Signature{KeyID: key.ID, Ed25519: encodeSignature(manifestSig)}
	if err := VerifyImageSignature(forged, keys); !errors.Is(err, ErrPackageSignatureInvalid) {
		t.Fatalf("manifest-signature replay error = %v, want ErrPackageSignatureInvalid", err)
	}

	// A signed package with a broken image signature is rejected by Verify.
	pkg, err := Sign(tampered, signingKey)
	if err != nil {
		t.Fatalf("sign tampered package: %v", err)
	}
	if err := Verify(pkg, keys); !errors.Is(err, ErrPackageSignatureInvalid) {
		t.Fatalf("verify tampered package error = %v, want ErrPackageSignatureInvalid", err)
	}

	// signedImageDigest without an image signature is structurally invalid.
	partial := testManifest()
	partial.SignedImageDigest = Digest{Algorithm: "sha256", Hex: strings.Repeat("c", 64)}
	if err := partial.Validate(); !errors.Is(err, ErrPackageManifestInvalid) {
		t.Fatalf("partial image binding error = %v, want ErrPackageManifestInvalid", err)
	}
}

func encodeSignature(signature []byte) string {
	return base64.RawStdEncoding.EncodeToString(signature)
}

func TestGenerateAndVerifySBOM(t *testing.T) {
	manifest := testManifest()
	manifest.RuntimeLock = []Digest{{Algorithm: "sha256", Hex: strings.Repeat("a", 64)}}
	manifest.ToolLock = []Digest{{Algorithm: "sha256", Hex: strings.Repeat("b", 64)}}
	document, err := GenerateSBOM(manifest)
	if err != nil {
		t.Fatalf("generate sbom: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(document, &parsed); err != nil {
		t.Fatalf("sbom is not JSON: %v", err)
	}
	if parsed["bomFormat"] != "CycloneDX" || parsed["specVersion"] != SBOMSpecVersion {
		t.Fatalf("sbom header = %+v", parsed)
	}
	digest := SBOMDigest(document)
	if err := VerifySBOM(document, digest); err != nil {
		t.Fatalf("verify sbom: %v", err)
	}

	// Tampered SBOM content fails verification.
	tampered := strings.Replace(string(document), "CycloneDX", "CycloneDX-tampered", 1)
	if err := VerifySBOM([]byte(tampered), digest); !errors.Is(err, ErrPackageManifestInvalid) {
		t.Fatalf("tampered sbom error = %v, want ErrPackageManifestInvalid", err)
	}
	// A non-CycloneDX document fails even with a matching digest.
	other := []byte(`{"bomFormat":"SPDX","specVersion":"2.3"}`)
	if err := VerifySBOM(other, SBOMDigest(other)); !errors.Is(err, ErrPackageManifestInvalid) {
		t.Fatalf("non-cyclonedx error = %v, want ErrPackageManifestInvalid", err)
	}
}
