package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Version != "1.0" {
		t.Fatalf("expected version 1.0, got %s", cfg.Version)
	}
	if cfg.Environment != "development" {
		t.Fatalf("expected development, got %s", cfg.Environment)
	}
	if cfg.LLM.DefaultProvider != "openrouter" {
		t.Fatalf("expected openrouter, got %s", cfg.LLM.DefaultProvider)
	}
	if cfg.Kernel.Budget.MaxCostUSD != 1.00 {
		t.Fatalf("expected 1.00, got %f", cfg.Kernel.Budget.MaxCostUSD)
	}
}

func TestHierarchicalYAMLMerge(t *testing.T) {
	tmpDir := t.TempDir()
	yamlPath := filepath.Join(tmpDir, "agent.yaml")

	yamlData := `version: "1.1"
environment: "staging"
llm:
  default_provider: "deepseek"
  providers:
    deepseek:
      model: "deepseek-coder"
      api_key: "sk-test-deepseek-key-12345"
      base_url: "https://api.deepseek.com/v1"
kernel:
  budget:
    max_cost_usd: 5.50
`
	if err := os.WriteFile(yamlPath, []byte(yamlData), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	if err := mergeYAMLFile(cfg, yamlPath); err != nil {
		t.Fatalf("merge failed: %v", err)
	}

	if cfg.Version != "1.1" {
		t.Errorf("expected 1.1, got %s", cfg.Version)
	}
	if cfg.Environment != "staging" {
		t.Errorf("expected staging, got %s", cfg.Environment)
	}
	if cfg.LLM.DefaultProvider != "deepseek" {
		t.Errorf("expected deepseek, got %s", cfg.LLM.DefaultProvider)
	}
	p := cfg.CurrentProvider()
	if p.Model != "deepseek-coder" {
		t.Errorf("expected deepseek-coder, got %s", p.Model)
	}
	if p.APIKey != "sk-test-deepseek-key-12345" {
		t.Errorf("expected key match, got %s", p.APIKey)
	}
	if cfg.Kernel.Budget.MaxCostUSD != 5.50 {
		t.Errorf("expected 5.50, got %f", cfg.Kernel.Budget.MaxCostUSD)
	}
}

func TestEnvironmentVariablePrecedence(t *testing.T) {
	cfg := DefaultConfig()
	os.Setenv("AGENT_LLM_PROVIDER", "qwen")
	os.Setenv("AGENT_LLM_MODEL", "qwen-max")
	os.Setenv("AGENT_LLM_API_KEY", "sk-qwen-env-override-999")
	os.Setenv("AGENT_BUDGET_USD", "8.88")
	defer func() {
		os.Unsetenv("AGENT_LLM_PROVIDER")
		os.Unsetenv("AGENT_LLM_MODEL")
		os.Unsetenv("AGENT_LLM_API_KEY")
		os.Unsetenv("AGENT_BUDGET_USD")
	}()

	applyEnvOverrides(cfg)

	if cfg.LLM.DefaultProvider != "qwen" {
		t.Errorf("expected qwen, got %s", cfg.LLM.DefaultProvider)
	}
	curr := cfg.CurrentProvider()
	if curr.Model != "qwen-max" {
		t.Errorf("expected qwen-max, got %s", curr.Model)
	}
	if curr.APIKey != "sk-qwen-env-override-999" {
		t.Errorf("expected sk-qwen-env-override-999, got %s", curr.APIKey)
	}
	if cfg.Kernel.Budget.MaxCostUSD != 8.88 {
		t.Errorf("expected 8.88, got %f", cfg.Kernel.Budget.MaxCostUSD)
	}
}

func TestMaskAPIKey(t *testing.T) {
	if got := MaskAPIKey(""); got != "[not set]" {
		t.Errorf("unexpected: %s", got)
	}
	if got := MaskAPIKey("12345"); got != "******" {
		t.Errorf("unexpected: %s", got)
	}
	if got := MaskAPIKey("sk-or-v1-abcdefgh1234"); got != "sk-or-...1234" {
		t.Errorf("unexpected: %s", got)
	}
}

func TestApplyConfigSet(t *testing.T) {
	cfg := DefaultConfig()
	applyConfigSet(cfg, "llm.default_provider", "ollama")
	if cfg.LLM.DefaultProvider != "ollama" {
		t.Errorf("expected ollama, got %s", cfg.LLM.DefaultProvider)
	}

	applyConfigSet(cfg, "llm.providers.ollama.model", "llama3:8b")
	if cfg.LLM.Providers["ollama"].Model != "llama3:8b" {
		t.Errorf("expected llama3:8b, got %s", cfg.LLM.Providers["ollama"].Model)
	}

	applyConfigSet(cfg, "kernel.budget.max_cost_usd", "3.14")
	if cfg.Kernel.Budget.MaxCostUSD != 3.14 {
		t.Errorf("expected 3.14, got %f", cfg.Kernel.Budget.MaxCostUSD)
	}
}
