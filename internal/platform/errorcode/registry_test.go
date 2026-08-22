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
