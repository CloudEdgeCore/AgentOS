// Package workload defines the bounded task specification understood by v0.1
// admission and placement. Unknown fields remain available to later stages.
package workload

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// imageDigestPattern bounds digests to "sha256:<64 lowercase hex>".
var imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type Spec struct {
	Priority    int         `json:"priority"`
	Deadline    *time.Time  `json:"deadline,omitempty"`
	Budget      Budget      `json:"budget"`
	Placement   Placement   `json:"placement"`
	RetryPolicy RetryPolicy `json:"retryPolicy,omitempty"`
	// Image pins the OCI image this workload must run in (ADR-010). Runtime
	// providers for container classes (oci/microvm) must run exactly this
	// reference; digest-only references are the production contract.
	Image *Image `json:"image,omitempty"`
	// ToolCalls is the deterministic tool script the reference provider
	// executes through the Tool Gateway before completing. Other providers
	// ignore it; the gateway enforces policy and budget regardless of caller.
	ToolCalls []ToolCall `json:"tools,omitempty"`
	// ModelCalls is the deterministic model script the reference provider
	// executes through the Model Gateway (Begin/Settle/Finish) after tools.
	ModelCalls []ModelCallScript `json:"modelCalls,omitempty"`
}

// Image is the digest-pinned container reference of a workload.
type Image struct {
	// Ref is the image reference the provider must run, e.g.
	// "example.com/agent@sha256:...". The worker verifies it against the
	// runtime's own configuration before starting anything.
	Ref string `json:"ref"`
	// Digest pins the image content ("sha256:<64 hex>"); empty allows a
	// mutable reference in dev, but production admission requires it for
	// container runtime classes.
	Digest string `json:"digest,omitempty"`
}

// Validate checks the pin shape: a non-empty reference and, when set, a
// canonical sha256 digest.
func (i Image) Validate() error {
	if strings.TrimSpace(i.Ref) == "" {
		return fmt.Errorf("image ref is required")
	}
	if strings.TrimSpace(i.Digest) != "" && !imageDigestPattern.MatchString(i.Digest) {
		return fmt.Errorf("image digest must be sha256:<64 lowercase hex>")
	}
	return nil
}

// Pinned reports whether the reference is content-addressed.
func (i Image) Pinned() bool { return imageDigestPattern.MatchString(i.Digest) }

// Canonical returns the pin the worker and admission compare: ref@digest when
// pinned, ref otherwise.
func (i Image) Canonical() string {
	if i.Pinned() {
		return i.Ref + "@" + i.Digest
	}
	return i.Ref
}

// Equal reports whether two pins denote the same image.
func (i Image) Equal(other Image) bool { return i.Canonical() == other.Canonical() }

// ModelCallScript is one deterministic model invocation with pre-declared
// usage, metered and hard-stopped by the Model Gateway.
type ModelCallScript struct {
	ModelRef       string `json:"modelRef"`
	InputTokens    int64  `json:"inputTokens,omitempty"`
	OutputTokens   int64  `json:"outputTokens,omitempty"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// ToolCall is one deterministic gateway invocation the reference provider
// performs on behalf of the Attempt. IdempotencyKeys must be unique within a
// task so replays converge on the stored receipts.
type ToolCall struct {
	ToolName       string          `json:"name"`
	ToolVersion    string          `json:"version,omitempty"`
	Action         string          `json:"action"`
	Resource       string          `json:"resource"`
	Args           json.RawMessage `json:"args,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey"`
}

type RetryPolicy struct {
	MaxAttempts int `json:"maxAttempts,omitempty"`
}

func (p RetryPolicy) EffectiveMaxAttempts() int {
	if p.MaxAttempts == 0 {
		return 3
	}
	return p.MaxAttempts
}

type Budget struct {
	Tokens      int64   `json:"tokens"`
	CostUSD     float64 `json:"costUsd"`
	ToolCalls   int64   `json:"toolCalls"`
	WallSeconds int64   `json:"wallSeconds"`
}

// Zero reports whether every dimension is unset, meaning the task carries no
// budget ceiling and its usage is not enforced.
func (b Budget) Zero() bool {
	return b.Tokens == 0 && b.CostUSD == 0 && b.ToolCalls == 0 && b.WallSeconds == 0
}

type Placement struct {
	RuntimeClasses []string `json:"runtimeClasses"`
	PreferredClass string   `json:"preferredClass,omitempty"`
	Region         string   `json:"region"`
	DataResidency  string   `json:"dataResidency,omitempty"`
	ArtifactRegion string   `json:"artifactRegion,omitempty"`
	CPU            int64    `json:"cpuMillis"`
	Memory         int64    `json:"memoryMiB"`
	LLMConcurrency int      `json:"llmConcurrency"`
}

func Decode(raw json.RawMessage) (Spec, error) {
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, fmt.Errorf("decode workload spec: %w", err)
	}
	return spec, nil
}
