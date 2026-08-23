package provider

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistrySeparatesLogicalModelAndProviderRouting(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(Config{Name: "deepseek", BaseURL: "https://models.example/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterRoute(Route{
		ModelRef: "logical/fast", Provider: "deepseek", Model: "DeepSeek-V4-Flash-w8a8-mtp",
	}); err != nil {
		t.Fatal(err)
	}
	executor, wireModel, err := registry.ResolveModel("logical/fast")
	if err != nil || executor == nil || wireModel != "DeepSeek-V4-Flash-w8a8-mtp" {
		t.Fatalf("logical route: executor=%v model=%q err=%v", executor != nil, wireModel, err)
	}
	_, fallbackModel, err := registry.ResolveModel("deepseek/direct-model")
	if err != nil || fallbackModel != "direct-model" {
		t.Fatalf("legacy route: model=%q err=%v", fallbackModel, err)
	}
	if err := registry.RegisterRoute(Route{ModelRef: "logical/missing", Provider: "absent", Model: "x"}); err == nil {
		t.Fatal("route accepted an unknown provider")
	}
}

// P2-05: duplicate provider names and duplicate model routes fail fast
// instead of the last file entry silently winning.
func TestRegistryRejectsDuplicateProvidersAndRoutes(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(Config{Name: "deepseek", BaseURL: "https://models.example/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(Config{Name: "deepseek", BaseURL: "https://other.example/v1"}); err == nil {
		t.Fatal("duplicate provider name accepted")
	}
	if err := registry.RegisterRoute(Route{ModelRef: "logical/fast", Provider: "deepseek", Model: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterRoute(Route{ModelRef: "logical/fast", Provider: "deepseek", Model: "m2"}); err == nil {
		t.Fatal("duplicate model route accepted")
	}
	executor, wireModel, err := registry.ResolveModel("logical/fast")
	if err != nil || executor == nil || wireModel != "m1" {
		t.Fatalf("route overridden: model=%q err=%v; the first declaration must survive a rejected duplicate", wireModel, err)
	}
}

func TestLoadRegistryFileRejectsDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": [
			{"name": "deepseek", "baseUrl": "https://models.example/v1"},
			{"name": "deepseek", "baseUrl": "https://other.example/v1"}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistryFile(path); err == nil {
		t.Fatal("duplicate providers in the configuration file accepted")
	}

	routePath := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(routePath, []byte(`{
		"providers": [{"name": "deepseek", "baseUrl": "https://models.example/v1"}],
		"routes": [
			{"modelRef": "logical/fast", "provider": "deepseek", "model": "m1"},
			{"modelRef": "logical/fast", "provider": "deepseek", "model": "m2"}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistryFile(routePath); err == nil {
		t.Fatal("duplicate routes in the configuration file accepted")
	}
}
