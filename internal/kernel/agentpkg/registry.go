package agentpkg

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrPackageBindingMismatch reports a package whose signed manifest does not
// bind to the publication it is attached to (reference or spec mismatch).
var ErrPackageBindingMismatch = errors.New("agent package does not bind to this publication")

// Registry is the trusted package-signing key set used by publish admission
// (ADR-010). Lookup is fail-closed: a package signed by a key that is not in
// the registry is rejected before any other check.
type Registry struct {
	mu   sync.RWMutex
	keys map[string]ed25519.PublicKey
}

// NewRegistry returns an empty trusted-key registry.
func NewRegistry() *Registry {
	return &Registry{keys: map[string]ed25519.PublicKey{}}
}

// Add trusts the given key identity. Re-adding an identity with a different
// key is an error: key IDs must be unique within the trust root.
func (r *Registry) Add(key Key) error {
	if strings.TrimSpace(key.ID) == "" {
		return fmt.Errorf("package key ID is required")
	}
	if len(key.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("package public key for %q has length %d", key.ID, len(key.PublicKey))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.keys[key.ID]; ok && !equalPublicKey(existing, key.PublicKey) {
		return fmt.Errorf("package key identity %q is already trusted with a different key", key.ID)
	}
	r.keys[key.ID] = key.PublicKey
	return nil
}

// Verify checks the package signature against the trusted registry,
// fail-closed: an unsigned package, an untrusted key, a malformed signature
// or a manifest mismatch rejects the package.
func (r *Registry) Verify(pkg *Package) error {
	if pkg == nil {
		return ErrPackageUnsigned
	}
	r.mu.RLock()
	trusted := make(map[string]ed25519.PublicKey, len(r.keys))
	for id, key := range r.keys {
		trusted[id] = key
	}
	r.mu.RUnlock()
	return Verify(pkg, trusted)
}

// Trusted returns a copy of the trusted key set.
func (r *Registry) Trusted() map[string]ed25519.PublicKey {
	r.mu.RLock()
	defer r.mu.RUnlock()
	copied := make(map[string]ed25519.PublicKey, len(r.keys))
	for id, key := range r.keys {
		copied[id] = key
	}
	return copied
}

// Len reports the number of trusted key identities.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.keys)
}

// EncodePublicKey renders a public key for transport (CLI flags, env vars).
func EncodePublicKey(key ed25519.PublicKey) string {
	return base64.RawStdEncoding.EncodeToString(key)
}

// DecodePublicKey parses a base64 raw std public key.
func DecodePublicKey(encoded string) (ed25519.PublicKey, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode package public key: %w", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("package public key has length %d, want %d", len(decoded), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(decoded), nil
}

// DecodePrivateKey parses a base64 raw std private key for the signing CLI.
func DecodePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode package private key: %w", err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("package private key has length %d, want %d", len(decoded), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(decoded), nil
}

func equalPublicKey(a, b ed25519.PublicKey) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
