package provider

import "testing"

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
