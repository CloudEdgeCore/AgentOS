package agentversion

import (
	"encoding/json"
	"strings"
	"testing"
)

func validManifest() Manifest {
	return Manifest{
		APIVersion: ManifestAPIVersion,
		Kind:       ManifestKind,
		Metadata: Metadata{
			Name: "research-agent", Version: "1.0.0", Namespace: "default",
		},
		Spec: Spec{
			RuntimeClassPolicy: RuntimeClassPolicy{Allowed: []string{"oci"}, Preferred: "oci"},
			Runtimes: []RuntimeTarget{{
				Class: "oci", Interface: RuntimeInterfaceV1Alpha1,
				RuntimeABI: "agentos.oci/v1", Entrypoint: []string{"/agent/bin/research", "serve"},
			}},
			Capabilities: &Capabilities{
				Tools: []string{"search@v1"}, Models: []string{"quality"},
				Memory: []string{"project/*"}, Secrets: []string{},
			},
			Resources:  &ResourceLimits{CPUMillis: 500, MemoryMiB: 512, WorkspaceBytes: 64 << 20},
			Budget:     &Budget{Tokens: 100_000, CostUSD: 10, ToolCalls: 100, WallSeconds: 3600},
			Checkpoint: &CheckpointPolicy{Mode: CheckpointLogical, SchemaVersion: "research-state/v1", IntervalSeconds: 30},
			Lifecycle:  Lifecycle{MaxAttempts: 3},
			Image: &Image{
				Ref:    "registry.example/research-agent",
				Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
	}
}

func TestDecodeManifestCanonicalRoundTrip(t *testing.T) {
	raw, err := json.MarshalIndent(validManifest(), "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	manifest, canonical, digest, err := DecodeManifest(raw)
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Ref() != "research-agent@1.0.0" {
		t.Fatalf("manifest ref = %q", manifest.Ref())
	}
	_, replay, replayDigest, err := DecodeManifest(canonical)
	if err != nil {
		t.Fatalf("decode canonical manifest: %v", err)
	}
	if string(canonical) != string(replay) || digest != replayDigest {
		t.Fatalf("canonical replay changed: %s vs %s", canonical, replay)
	}
}

func TestDecodeManifestRejectsAmbiguousAndUnknownJSON(t *testing.T) {
	raw, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	duplicate := strings.Replace(string(raw), `"kind":"AgentManifest"`, `"kind":"AgentManifest","kind":"AgentManifest"`, 1)
	unknown := strings.Replace(string(raw), `"metadata":{`, `"unexpected":true,"metadata":{`, 1)
	for name, encoded := range map[string]string{
		"duplicate key":  duplicate,
		"unknown field":  unknown,
		"trailing value": string(raw) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := DecodeManifest([]byte(encoded)); err == nil {
				t.Fatalf("DecodeManifest unexpectedly accepted %s", encoded)
			}
		})
	}
}

func TestManifestRequiresExplicitDefaultDenyAndLimits(t *testing.T) {
	for name, mutate := range map[string]func(*Manifest){
		"capabilities": func(manifest *Manifest) { manifest.Spec.Capabilities = nil },
		"resources":    func(manifest *Manifest) { manifest.Spec.Resources = nil },
		"budget":       func(manifest *Manifest) { manifest.Spec.Budget = nil },
		"checkpoint":   func(manifest *Manifest) { manifest.Spec.Checkpoint = nil },
	} {
		t.Run(name, func(t *testing.T) {
			manifest := validManifest()
			mutate(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatalf("manifest without %s was accepted", name)
			}
		})
	}
}

func TestManifestRuntimeAndCapabilityValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Manifest){
		"runtime class mismatch": func(manifest *Manifest) {
			manifest.Spec.RuntimeClassPolicy.Allowed = []string{"wasmtime"}
		},
		"duplicate runtime": func(manifest *Manifest) {
			manifest.Spec.Runtimes = append(manifest.Spec.Runtimes, manifest.Spec.Runtimes[0])
			manifest.Spec.RuntimeClassPolicy.Allowed = []string{"oci", "oci"}
		},
		"wrong interface": func(manifest *Manifest) {
			manifest.Spec.Runtimes[0].Interface = "agentos.runtime/v2"
		},
		"empty entrypoint": func(manifest *Manifest) {
			manifest.Spec.Runtimes[0].Entrypoint = nil
		},
		"duplicate capability": func(manifest *Manifest) {
			manifest.Spec.Capabilities.Tools = []string{"search@v1", "search@v1"}
		},
		"omitted capability class": func(manifest *Manifest) {
			manifest.Spec.Capabilities.Secrets = nil
		},
		"zero container workspace": func(manifest *Manifest) {
			manifest.Spec.Resources.WorkspaceBytes = 0
		},
		"invalid checkpoint": func(manifest *Manifest) {
			manifest.Spec.Checkpoint.Mode = CheckpointNone
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := validManifest()
			mutate(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatalf("invalid manifest %q was accepted", name)
			}
		})
	}
}

func TestValidateSpecChecksPlatformFieldsWhenPresent(t *testing.T) {
	manifest := validManifest()
	encoded, err := json.Marshal(manifest.Spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if err := ValidateSpec(encoded); err != nil {
		t.Fatalf("full platform spec rejected: %v", err)
	}

	manifest.Spec.Budget.WallSeconds = 0
	encoded, err = json.Marshal(manifest.Spec)
	if err != nil {
		t.Fatalf("marshal invalid spec: %v", err)
	}
	if err := ValidateSpec(encoded); err == nil {
		t.Fatal("invalid platform budget was accepted")
	}
}
