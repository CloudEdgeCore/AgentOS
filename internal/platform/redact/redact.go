// Package redact contains cross-cutting trust-boundary sanitizers.
package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
)

const Replacement = "[REDACTED]"

var (
	bearerPattern     = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]+`)
	assignmentPattern = regexp.MustCompile(`(?i)(\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|authorization|password|passwd|secret|signature)\b\s*[:=]\s*["']?)[^\s,"';}&]+`)
	urlPattern        = regexp.MustCompile(`https?://[^\s"'<>]+`)
)

// RedactText removes common credentials, credential-bearing URLs, and every
// caller-supplied secret literal from untrusted text before it crosses a log,
// event, result, artifact, or protocol error boundary.
func RedactText(value string, secrets ...string) string {
	redacted := value
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, Replacement)
		}
	}
	redacted = bearerPattern.ReplaceAllString(redacted, "${1}"+Replacement)
	redacted = assignmentPattern.ReplaceAllString(redacted, "${1}"+Replacement)
	redacted = urlPattern.ReplaceAllStringFunc(redacted, redactURL)
	return redacted
}

// RedactJSON recursively redacts sensitive field values and credential-like
// strings. Invalid JSON is rejected so callers cannot accidentally publish a
// truncated or partially redacted document.
func RedactJSON(raw []byte, secrets ...string) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, false
	}
	redactValue(&value, secrets)
	encoded, err := json.Marshal(value)
	return encoded, err == nil
}

func redactValue(value *any, secrets []string) {
	switch current := (*value).(type) {
	case string:
		*value = RedactText(current, secrets...)
	case []any:
		for index := range current {
			redactValue(&current[index], secrets)
		}
	case map[string]any:
		for key, child := range current {
			if sensitiveKey(key) {
				current[key] = Replacement
				continue
			}
			redactValue(&child, secrets)
			current[key] = child
		}
	}
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	switch normalized {
	case "key", "apikey", "token", "accesstoken", "refreshtoken", "auth", "authorization", "password", "passwd", "secret", "clientsecret", "signature":
		return true
	default:
		return false
	}
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "[REDACTED_URL]"
	}
	if parsed.User != nil {
		parsed.User = url.User(Replacement)
	}
	query := parsed.Query()
	for key := range query {
		if sensitiveKey(key) {
			query.Set(key, Replacement)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
