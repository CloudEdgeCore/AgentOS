package redact

import (
	"strings"
	"testing"
)

func TestRedactTextCoversCredentialsAndURLs(t *testing.T) {
	input := `Bearer abc.def api_key=top-secret https://user:pass@example.test/v1?token=query-secret&ok=1 literal-value`
	redacted := RedactText(input, "literal-value", "top-secret")
	for _, secret := range []string{"abc.def", "top-secret", "user:pass", "query-secret", "literal-value"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("secret %q survived: %s", secret, redacted)
		}
	}
}

func TestRedactJSONPreservesShapeAndRedactsNestedValues(t *testing.T) {
	redacted, ok := RedactJSON([]byte(`{"apiKey":"secret-key","nested":{"message":"Bearer token-value","safe":7}}`))
	if !ok || strings.Contains(string(redacted), "secret-key") || strings.Contains(string(redacted), "token-value") || !strings.Contains(string(redacted), `"safe":7`) {
		t.Fatalf("redacted=%s ok=%v", redacted, ok)
	}
	if _, ok := RedactJSON([]byte(`{"broken"`)); ok {
		t.Fatal("invalid JSON accepted")
	}
}
