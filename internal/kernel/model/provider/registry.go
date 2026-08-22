package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// Registry resolves provider names (the first path segment of a model
// reference, e.g. "deepseek" in deepseek/deepseek-chat) to their executors.
// An empty registry fails closed: every reference resolves to an explicit
// "no execution endpoint configured" error rather than silently bypassing
// the Model Gateway.
type Registry struct {
	mu        sync.RWMutex
	executors map[string]*Executor
}

// NewRegistry builds an empty registry. client may be nil (a default HTTP
// client is used per executor).
func NewRegistry() *Registry {
	return &Registry{executors: map[string]*Executor{}}
}

// Register adds or replaces one provider endpoint configuration.
func (r *Registry) Register(config Config) error {
	if strings.TrimSpace(config.Name) == "" {
		return fmt.Errorf("provider name is required")
	}
	if strings.TrimSpace(config.BaseURL) == "" {
		return fmt.Errorf("provider %q: base URL is required", config.Name)
	}
	if !strings.HasPrefix(config.BaseURL, "https://") && !strings.HasPrefix(config.BaseURL, "http://") {
		return fmt.Errorf("provider %q: base URL must be absolute", config.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.executors[config.Name] = NewExecutor(config, nil)
	return nil
}

// Resolve returns the executor for one provider name.
func (r *Registry) Resolve(name string) (*Executor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	executor, ok := r.executors[name]
	if !ok {
		return nil, fmt.Errorf("model provider %q has no execution endpoint configured", name)
	}
	return executor, nil
}

// Names lists registered provider names in stable order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.executors))
	for name := range r.executors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// registryFile is the on-disk shape of the -model-providers configuration:
//
//	{
//	  "providers": [
//	    {"name": "deepseek", "baseUrl": "https://api.deepseek.com/v1",
//	     "apiKeyEnv": "DEEPSEEK_API_KEY", "timeoutMs": 120000,
//	     "maxAttempts": 3, "breakerOpens": 5, "breakerCooldownMs": 30000},
//	    {"name": "qwen", "baseUrl": "https://dashscope.example/v1",
//	     "apiKeyEnv": "QWEN_API_KEY"}
//	  ]
//	}
//
// Every provider speaks the OpenAI-compatible /chat/completions dialect.
// API keys are referenced by environment variable name only and are never
// persisted.
type registryFile struct {
	Providers []Config `json:"providers"`
}

// LoadRegistryFile reads a provider configuration file. An empty path yields
// an empty (fail-closed) registry.
func LoadRegistryFile(path string) (*Registry, error) {
	registry := NewRegistry()
	if strings.TrimSpace(path) == "" {
		return registry, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read model providers file: %w", err)
	}
	var file registryFile
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode model providers file: %w", err)
	}
	for _, config := range file.Providers {
		if config.APIKey != "" {
			return nil, fmt.Errorf("provider %q: apiKey must not be embedded in the file; use apiKeyEnv", config.Name)
		}
		if err := registry.Register(config); err != nil {
			return nil, err
		}
	}
	return registry, nil
}
