package errorcode

import (
	"regexp"
	"testing"
)

func TestRegistryContainsUniqueStableCodes(t *testing.T) {
	pattern := regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)
	seen := map[string]bool{}
	for _, code := range All() {
		if seen[code] || !pattern.MatchString(code) || !Known(code) {
			t.Fatalf("invalid or duplicate public error code %q", code)
		}
		seen[code] = true
	}
}

// P3-04: every stable error code carries exactly one public disposition, and
// the disposition table never drifts from the registry — an unknown code
// reports not-ok so callers fail closed instead of guessing a retry policy.
func TestClassOfCoversEveryCodeExactlyOnce(t *testing.T) {
	for _, code := range All() {
		class, ok := ClassOf(code)
		if !ok {
			t.Fatalf("code %q has no disposition", code)
		}
		switch class {
		case Retryable, Terminal, UserActionRequired, OperatorActionRequired:
		default:
			t.Fatalf("code %q has unknown class %q", code, class)
		}
	}
	for _, code := range []string{"NO_SUCH_CODE", "", "internal_error"} {
		if _, ok := ClassOf(code); ok {
			t.Fatalf("unknown code %q resolved a class; want fail-closed not-ok", code)
		}
	}
	retryable, ok := ClassOf(ProviderUnavailable)
	if !ok || retryable != Retryable {
		t.Fatalf("PROVIDER_UNAVAILABLE = %q (ok=%v), want retryable", retryable, ok)
	}
}
