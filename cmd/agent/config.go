package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ProviderConfig 定义每个模型提供商的独立参数
type ProviderConfig struct {
	Model      string `yaml:"model" json:"model"`
	APIKey     string `yaml:"api_key" json:"api_key"`
	BaseURL    string `yaml:"base_url" json:"base_url"`
	TimeoutSec int    `yaml:"timeout_sec,omitempty" json:"timeout_sec,omitempty"`
}

// LLMConfig 定义大模型分级配置
type LLMConfig struct {
	DefaultProvider string                    `yaml:"default_provider" json:"default_provider"`
	Providers       map[string]ProviderConfig `yaml:"providers" json:"providers"`
}

// BudgetConfig 定义内核预算
type BudgetConfig struct {
	MaxCostUSD   float64 `yaml:"max_cost_usd" json:"max_cost_usd"`
	MaxTokens    int     `yaml:"max_tokens" json:"max_tokens"`
	MaxToolCalls int     `yaml:"max_tool_calls" json:"max_tool_calls"`
}

// GovernanceConfig 定义内核安全合规
type GovernanceConfig struct {
	EnforceReceipts bool `yaml:"enforce_receipts" json:"enforce_receipts"`
	FailClosed      bool `yaml:"fail_closed" json:"fail_closed"`
}

// KernelConfig 定义内核管控分级
type KernelConfig struct {
	Budget     BudgetConfig     `yaml:"budget" json:"budget"`
	Governance GovernanceConfig `yaml:"governance" json:"governance"`
}

// GatewayConfig 定义网关分级
type GatewayConfig struct {
	Port int    `yaml:"port" json:"port"`
	Host string `yaml:"host" json:"host"`
}

// LoggingConfig 定义日志追踪分级
type LoggingConfig struct {
	Level  string `yaml:"level" json:"level"`
	Format string `yaml:"format" json:"format"`
}

// ProfileOverride 定义环境差异化覆盖
type ProfileOverride struct {
	Kernel  *KernelConfig  `yaml:"kernel,omitempty" json:"kernel,omitempty"`
	Logging *LoggingConfig `yaml:"logging,omitempty" json:"logging,omitempty"`
}

// AgentYAMLConfig 顶层完整分级配置
type AgentYAMLConfig struct {
	Version     string                     `yaml:"version" json:"version"`
	Environment string                     `yaml:"environment" json:"environment"`
	LLM         LLMConfig                  `yaml:"llm" json:"llm"`
	Kernel      KernelConfig               `yaml:"kernel" json:"kernel"`
	Gateway     GatewayConfig              `yaml:"gateway" json:"gateway"`
	Logging     LoggingConfig              `yaml:"logging" json:"logging"`
	Profiles    map[string]ProfileOverride `yaml:"profiles,omitempty" json:"profiles,omitempty"`

	// 运行时元数据 (不序列化)
	LoadedFiles []string `yaml:"-" json:"-"`
}

var activeConfig *AgentYAMLConfig

// DefaultConfig 提供系统底座默认值
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

// LoadConfig 实现严格的分级加载规则 (Cascading Configuration)
// 层级优先级:
// Level 1: 内置默认值
// Level 2: 全局级 (~/.agent/agent.yaml)
// Level 3: 项目级 (./agent.yaml 或上级目录)
// Level 4: 环境 Profile 覆盖 (如 profiles[cfg.Environment])
// Level 5: 环境变量覆盖 (AGENT_LLM_KEY, LLM_API_KEY 等)
func LoadConfig() (*AgentYAMLConfig, error) {
	if activeConfig != nil {
		return activeConfig, nil
	}

	cfg := DefaultConfig()

	// 1. 全局配置查找 (~/.agent/agent.yaml)
	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		globalYaml := filepath.Join(homeDir, ".agent", "agent.yaml")
		if err := mergeYAMLFile(cfg, globalYaml); err == nil {
			cfg.LoadedFiles = append(cfg.LoadedFiles, globalYaml)
		}
	}

	// 2. 本地项目配置查找 (./agent.yaml, ./config.yaml, .agent.yaml)
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

	// 3. 应用当前 Environment Profile 覆盖
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

	// 4. 环境变量覆盖 (最高优先级覆盖项)
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
	return nil
}

func applyEnvOverrides(cfg *AgentYAMLConfig) {
	// 支持 AGENT_ENV / AGENT_ENVIRONMENT
	if env := os.Getenv("AGENT_ENV"); env != "" {
		cfg.Environment = env
	}

	// 支持默认提供商覆盖
	if provider := os.Getenv("AGENT_LLM_PROVIDER"); provider != "" {
		cfg.LLM.DefaultProvider = provider
	} else if provider := os.Getenv("LLM_PROVIDER"); provider != "" {
		cfg.LLM.DefaultProvider = provider
	}

	curr := cfg.CurrentProvider()

	// 支持当前默认大模型参数环境变量覆盖
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

// CurrentProvider 获取当前激活生效的模型提供商参数
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

// SaveConfig 保存回当前工作的 agent.yaml
func SaveConfig(cfg *AgentYAMLConfig, targetPath string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(targetPath, data, 0644)
}

func MaskAPIKey(k string) string {
	if k == "" {
		return "[未设置 (未配置 API_KEY)]"
	}
	if len(k) <= 8 {
		return "******"
	}
	return k[:6] + "..." + k[len(k)-4:]
}
