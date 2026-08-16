package admission

import (
	"encoding/json"
	"testing"

	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

const pinnedDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func versionWithImage(imageJSON string) store.AgentVersion {
	spec := `{"runtimeClassPolicy":{"allowed":["oci"]}}`
	if imageJSON != "" {
		spec = `{"runtimeClassPolicy":{"allowed":["oci"]},"image":` + imageJSON + `}`
	}
	return store.AgentVersion{
		ID: uuid.New(), TenantID: "tenant-a", Name: "agent", Version: "1",
		Spec: json.RawMessage(spec),
	}
}

func pinnedImageJSON(ref, digest string) string {
	return `{"ref":"` + ref + `","digest":"` + digest + `"}`
}

func taskSpecWithImage(image string) json.RawMessage {
	body := `{"placement":{"runtimeClasses":["oci"]}}`
	if image != "" {
		body = `{"placement":{"runtimeClasses":["oci"]},"image":` + image + `}`
	}
	return json.RawMessage(body)
}

func reasonCodes(reasons []store.AdmissionReason) []string {
	codes := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		codes = append(codes, reason.Code)
	}
	return codes
}

func TestImagePinPolicyAdmitsMatchingPin(t *testing.T) {
	version := versionWithImage(pinnedImageJSON("example.com/agent", pinnedDigest))
	reasons := checkVersionPolicy(taskSpecWithImage(pinnedImageJSON("example.com/agent", pinnedDigest)), version)
	if len(reasons) != 0 {
		t.Fatalf("matching pin rejected: %v", reasonCodes(reasons))
	}
}

func TestImagePinPolicyRequiresPinWhenVersionPins(t *testing.T) {
	version := versionWithImage(pinnedImageJSON("example.com/agent", pinnedDigest))
	reasons := checkVersionPolicy(taskSpecWithImage(""), version)
	if !hasReason(reasons, "RUNTIME_IMAGE_REQUIRED") {
		t.Fatalf("missing pin not rejected: %v", reasonCodes(reasons))
	}
}

func TestImagePinPolicyRejectsMismatchedPin(t *testing.T) {
	version := versionWithImage(pinnedImageJSON("example.com/agent", pinnedDigest))
	reasons := checkVersionPolicy(taskSpecWithImage(pinnedImageJSON("example.com/evil", pinnedDigest)), version)
	if !hasReason(reasons, "RUNTIME_IMAGE_MISMATCH") {
		t.Fatalf("mismatched pin not rejected: %v", reasonCodes(reasons))
	}
	// Same ref, different digest is also a mismatch (content pin).
	otherDigest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	reasons = checkVersionPolicy(taskSpecWithImage(pinnedImageJSON("example.com/agent", otherDigest)), version)
	if !hasReason(reasons, "RUNTIME_IMAGE_MISMATCH") {
		t.Fatalf("digest mismatch not rejected: %v", reasonCodes(reasons))
	}
}

func TestImagePinPolicyRejectsMalformedTaskPin(t *testing.T) {
	// No version pin, but the task pin is malformed: fail closed.
	version := versionWithImage("")
	reasons := checkVersionPolicy(taskSpecWithImage(pinnedImageJSON("example.com/agent", "md5:abc")), version)
	if !hasReason(reasons, "RUNTIME_IMAGE_INVALID") {
		t.Fatalf("malformed pin not rejected: %v", reasonCodes(reasons))
	}
}

func hasReason(reasons []store.AdmissionReason, code string) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}
