package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ProviderConfig defines parameters for an LLM provider endpoint.
type ProviderConfig struct {
	Model      string `yaml:"model" json:"model"`
	APIKey     string `yaml:"api_key" json:"api_key"`
	BaseURL    string `yaml:"base_url" json:"base_url"`
	TimeoutSec int    `yaml:"timeout_sec,omitempty" json:"timeout_sec,omitempty"`
}

// LLMConfig defines provider selection and provider configurations.
type LLMConfig struct {
	DefaultProvider string                    `yaml:"default_provider" json:"default_provider"`
	Providers       map[string]ProviderConfig `yaml:"providers" json:"providers"`
}

// BudgetConfig defines kernel runtime resource budgets.
type BudgetConfig struct {
	MaxCostUSD   float64 `yaml:"max_cost_usd" json:"max_cost_usd"`
	MaxTokens    int     `yaml:"max_tokens" json:"max_tokens"`
	MaxToolCalls int     `yaml:"max_tool_calls" json:"max_tool_calls"`
}

// GovernanceConfig defines kernel compliance and enforcement rules.
type GovernanceConfig struct {
	EnforceReceipts bool `yaml:"enforce_receipts" json:"enforce_receipts"`
	FailClosed      bool `yaml:"fail_closed" json:"fail_closed"`
}

// KernelConfig defines kernel management tiers.
type KernelConfig struct {
	Budget     BudgetConfig     `yaml:"budget" json:"budget"`
	Governance GovernanceConfig `yaml:"governance" json:"governance"`
}

// GatewayConfig defines network gateway settings.
type GatewayConfig struct {
	Port int    `yaml:"port" json:"port"`
	Host string `yaml:"host" json:"host"`
}

// LoggingConfig defines logging level and output format.
type LoggingConfig struct {
	Level  string `yaml:"level" json:"level"`
	Format string `yaml:"format" json:"format"`
}

// ProfileOverride defines environment-specific configuration overrides.
type ProfileOverride struct {
	Kernel  *KernelConfig  `yaml:"kernel,omitempty" json:"kernel,omitempty"`
	Logging *LoggingConfig `yaml:"logging,omitempty" json:"logging,omitempty"`
}

// MCPToolConfig defines parameters for a registered Model Context Protocol tool.
type MCPToolConfig struct {
	Name        string `yaml:"name" json:"name"`
	Adapter     string `yaml:"adapter" json:"adapter"`
	Protocol    string `yaml:"protocol" json:"protocol"`
	Endpoint    string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Status      string `yaml:"status" json:"status"`
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	Description string `yaml:"description" json:"description"`
}

// AgentYAMLConfig is the top-level hierarchical configuration.
type AgentYAMLConfig struct {
	Version     string                     `yaml:"version" json:"version"`
	Environment string                     `yaml:"environment" json:"environment"`
	LLM         LLMConfig                  `yaml:"llm" json:"llm"`
	Kernel      KernelConfig               `yaml:"kernel" json:"kernel"`
	Gateway     GatewayConfig              `yaml:"gateway" json:"gateway"`
	Logging     LoggingConfig              `yaml:"logging" json:"logging"`
	MCPTools    []MCPToolConfig            `yaml:"mcp_tools,omitempty" json:"mcp_tools,omitempty"`
	Profiles    map[string]ProfileOverride `yaml:"profiles,omitempty" json:"profiles,omitempty"`

	// LoadedFiles tracks configuration precedence chain at runtime.
	LoadedFiles []string `yaml:"-" json:"-"`
}

var activeConfig *AgentYAMLConfig

// DefaultConfig provides baseline system defaults.
func DefaultConfig() *AgentYAMLConfig {
	return &AgentYAMLConfig{
		Version:     "1.0",
		Environment: "development",
		LLM: LLMConfig{
			DefaultProvider: "openrouter",
			Providers: map[string]ProviderConfig{
				"openrouter": {
					Model:      "stealth/space-bunny-alpha",
					APIKey:     "",
					BaseURL:    "https://openrouter.ai/api/v1",
					TimeoutSec: 180,
				},
				"deepseek": {
					Model:      "deepseek-chat",
					APIKey:     "",
					BaseURL:    "https://api.deepseek.com/v1",
					TimeoutSec: 120,
				},
				"qwen": {
					Model:      "qwen-plus",
					APIKey:     "",
					BaseURL:    "https://dashscope.aliyuncs.com/compatible-mode/v1",
					TimeoutSec: 120,
				},
				"ollama": {
					Model:      "qwen2.5:7b",
					APIKey:     "ollama",
					BaseURL:    "http://localhost:11434/v1",
					TimeoutSec: 120,
				},
			},
		},
		Kernel: KernelConfig{
			Budget: BudgetConfig{
				MaxCostUSD:   1.00,
				MaxTokens:    30000,
				MaxToolCalls: 15,
			},
			Governance: GovernanceConfig{
				EnforceReceipts: true,
				FailClosed:      true,
			},
		},
		Gateway: GatewayConfig{
			Port: 18080,
			Host: "127.0.0.1",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
		MCPTools: []MCPToolConfig{
			{
				Name:        "industrial:sensor",
				Adapter:     "industrial.sensor.query@1.0.0",
				Protocol:    "MCP/2.0",
				Status:      "HEALTHY",
				Enabled:     true,
				Description: "Query live industrial telemetry including spindle temperature, motor vibration, and cooling pressure.",
			},
			{
				Name:        "industrial:alarm",
				Adapter:     "industrial.alarm.lookup@1.0.0",
				Protocol:    "MCP/2.0",
				Status:      "HEALTHY",
				Enabled:     true,
				Description: "Look up equipment fault codes, alarm thresholds, and recommended hardware mitigations.",
			},
			{
				Name:        "industrial:sop",
				Adapter:     "industrial.sop.search@1.0.0",
				Protocol:    "MCP/2.0",
				Status:      "HEALTHY",
				Enabled:     true,
				Description: "Semantic search across standard operating procedures (SOP), safety guidelines, and work instructions.",
			},
			{
				Name:        "quality:metrics",
				Adapter:     "custom.quality.metrics@1.0.0",
				Protocol:    "Native/Go",
				Status:      "HEALTHY",
				Enabled:     true,
				Description: "Retrieve Statistical Process Control (SPC) metrics, Cp/Cpk indices, and defect rates.",
			},
			{
				Name:        "process:telemetry",
				Adapter:     "custom.process.telemetry@1.0.0",
				Protocol:    "Native/Go",
				Status:      "HEALTHY",
				Enabled:     true,
				Description: "Stream high-frequency process time-series data from edge collectors.",
			},
			{
				Name:        "knowledge:cases",
				Adapter:     "custom.knowledge.cases@1.0.0",
				Protocol:    "pgvector/SQL",
				Status:      "HEALTHY",
				Enabled:     true,
				Description: "Semantic vector similarity lookup against historical incident postmortems.",
			},
		},
		Profiles: map[string]ProfileOverride{
			"development": {
				Kernel: &KernelConfig{
					Budget: BudgetConfig{MaxCostUSD: 1.00, MaxTokens: 30000, MaxToolCalls: 15},
				},
			},
			"production": {
				Kernel: &KernelConfig{
					Budget: BudgetConfig{MaxCostUSD: 10.00, MaxTokens: 100000, MaxToolCalls: 50},
				},
			},
		},
		LoadedFiles: []string{},
	}
}

// LoadConfig implements cascading configuration loading:
// 1. Builtin defaults
// 2. Global file (~/.agent/agent.yaml)
// 3. Project file (./agent.yaml)
// 4. Environment profile override
// 5. Environment variables (AGENT_LLM_*, LLM_API_KEY, etc.)
func LoadConfig() (*AgentYAMLConfig, error) {
	if activeConfig != nil {
		return activeConfig, nil
	}

	cfg := DefaultConfig()

	// 1. Check global user configuration (~/.agent/agent.yaml)
	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		globalYaml := filepath.Join(homeDir, ".agent", "agent.yaml")
		if err := mergeYAMLFile(cfg, globalYaml); err == nil {
			cfg.LoadedFiles = append(cfg.LoadedFiles, globalYaml)
		}
	}

	// 2. Check local project directory (./agent.yaml, ./config.yaml, .agent.yaml)
	projectYamlPaths := []string{
		"agent.yaml",
		".agent.yaml",
		"config.yaml",
		filepath.Join("..", "agent.yaml"),
		filepath.Join("..", "..", "agent.yaml"),
	}
	for _, p := range projectYamlPaths {
		if err := mergeYAMLFile(cfg, p); err == nil {
			cfg.LoadedFiles = append(cfg.LoadedFiles, p)
			break
		}
	}

	// 3. Apply profile overrides for active environment
	if profile, ok := cfg.Profiles[cfg.Environment]; ok {
		if profile.Kernel != nil {
			if profile.Kernel.Budget.MaxCostUSD > 0 {
				cfg.Kernel.Budget.MaxCostUSD = profile.Kernel.Budget.MaxCostUSD
			}
			if profile.Kernel.Budget.MaxTokens > 0 {
				cfg.Kernel.Budget.MaxTokens = profile.Kernel.Budget.MaxTokens
			}
			if profile.Kernel.Budget.MaxToolCalls > 0 {
				cfg.Kernel.Budget.MaxToolCalls = profile.Kernel.Budget.MaxToolCalls
			}
		}
		if profile.Logging != nil {
			if profile.Logging.Level != "" {
				cfg.Logging.Level = profile.Logging.Level
			}
			if profile.Logging.Format != "" {
				cfg.Logging.Format = profile.Logging.Format
			}
		}
	}

	// 4. Apply environment variable overrides (highest precedence)
	applyEnvOverrides(cfg)

	activeConfig = cfg
	return cfg, nil
}

func mergeYAMLFile(cfg *AgentYAMLConfig, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var override AgentYAMLConfig
	if err := yaml.Unmarshal(data, &override); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	if override.Version != "" {
		cfg.Version = override.Version
	}
	if override.Environment != "" {
		cfg.Environment = override.Environment
	}
	if override.LLM.DefaultProvider != "" {
		cfg.LLM.DefaultProvider = override.LLM.DefaultProvider
	}
	if len(override.LLM.Providers) > 0 {
		if cfg.LLM.Providers == nil {
			cfg.LLM.Providers = make(map[string]ProviderConfig)
		}
		for k, v := range override.LLM.Providers {
			existing := cfg.LLM.Providers[k]
			if v.Model != "" {
				existing.Model = v.Model
			}
			if v.APIKey != "" {
				existing.APIKey = v.APIKey
			}
			if v.BaseURL != "" {
				existing.BaseURL = v.BaseURL
			}
			if v.TimeoutSec > 0 {
				existing.TimeoutSec = v.TimeoutSec
			}
			cfg.LLM.Providers[k] = existing
		}
	}
	if override.Kernel.Budget.MaxCostUSD > 0 {
		cfg.Kernel.Budget.MaxCostUSD = override.Kernel.Budget.MaxCostUSD
	}
	if override.Kernel.Budget.MaxTokens > 0 {
		cfg.Kernel.Budget.MaxTokens = override.Kernel.Budget.MaxTokens
	}
	if override.Kernel.Budget.MaxToolCalls > 0 {
		cfg.Kernel.Budget.MaxToolCalls = override.Kernel.Budget.MaxToolCalls
	}
	if override.Gateway.Port > 0 {
		cfg.Gateway.Port = override.Gateway.Port
	}
	if override.Gateway.Host != "" {
		cfg.Gateway.Host = override.Gateway.Host
	}
	if override.Logging.Level != "" {
		cfg.Logging.Level = override.Logging.Level
	}
	if override.Logging.Format != "" {
		cfg.Logging.Format = override.Logging.Format
	}
	if len(override.MCPTools) > 0 {
		cfg.MCPTools = override.MCPTools
	}
	if override.Kernel.Governance.EnforceReceipts != cfg.Kernel.Governance.EnforceReceipts || override.Kernel.Governance.FailClosed != cfg.Kernel.Governance.FailClosed {
		cfg.Kernel.Governance = override.Kernel.Governance
	}
	return nil
}

func applyEnvOverrides(cfg *AgentYAMLConfig) {
	if env := os.Getenv("AGENT_ENV"); env != "" {
		cfg.Environment = env
	}

	if provider := os.Getenv("AGENT_LLM_PROVIDER"); provider != "" {
		cfg.LLM.DefaultProvider = provider
	} else if provider := os.Getenv("LLM_PROVIDER"); provider != "" {
		cfg.LLM.DefaultProvider = provider
	}

	curr := cfg.CurrentProvider()

	if key := os.Getenv("AGENT_LLM_API_KEY"); key != "" {
		curr.APIKey = key
	} else if key := os.Getenv("LLM_API_KEY"); key != "" {
		curr.APIKey = key
	} else if key := os.Getenv("OPENROUTER_API_KEY"); key != "" && cfg.LLM.DefaultProvider == "openrouter" {
		curr.APIKey = key
	}

	if model := os.Getenv("AGENT_LLM_MODEL"); model != "" {
		curr.Model = model
	} else if model := os.Getenv("LLM_MODEL"); model != "" {
		curr.Model = model
	}

	if url := os.Getenv("AGENT_LLM_BASE_URL"); url != "" {
		curr.BaseURL = url
	} else if url := os.Getenv("LLM_BASE_URL"); url != "" {
		curr.BaseURL = url
	}

	if budgetStr := os.Getenv("AGENT_BUDGET_USD"); budgetStr != "" {
		if b, err := strconv.ParseFloat(budgetStr, 64); err == nil {
			cfg.Kernel.Budget.MaxCostUSD = b
		}
	}

	cfg.LLM.Providers[cfg.LLM.DefaultProvider] = curr
}

// CurrentProvider returns the parameters of the currently active model provider.
func (c *AgentYAMLConfig) CurrentProvider() ProviderConfig {
	if p, ok := c.LLM.Providers[c.LLM.DefaultProvider]; ok {
		return p
	}
	return ProviderConfig{
		Model:      "stealth/space-bunny-alpha",
		BaseURL:    "https://openrouter.ai/api/v1",
		TimeoutSec: 180,
	}
}

// SaveConfig persists the configuration to the target agent.yaml file.
func SaveConfig(cfg *AgentYAMLConfig, targetPath string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(targetPath, data, 0644)
}

func MaskAPIKey(k string) string {
	if k == "" {
		return "[not set]"
	}
	if len(k) <= 8 {
		return "******"
	}
	return k[:6] + "..." + k[len(k)-4:]
}
