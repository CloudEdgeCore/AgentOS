package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrProviderNotFound    = errors.New("provider not found")
	ErrProviderExists      = errors.New("provider already registered")
	ErrInvalidProviderType = errors.New("invalid provider type")
)

// Registry manages the set of available providers across all categories.
type Registry struct {
	mu        sync.RWMutex
	providers map[ProviderType]map[string]Provider
}

// NewRegistry initializes a clean, thread-safe Provider Registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[ProviderType]map[string]Provider),
	}
}

// DefaultRegistry is the global in-process provider registry.
var DefaultRegistry = NewRegistry()

// Register adds a provider to the registry after validating its manifest.
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return errors.New("provider cannot be nil")
	}
	m := p.Manifest()
	if err := m.Validate(); err != nil {
		return fmt.Errorf("register provider failed: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	bucket, exists := r.providers[m.Type]
	if !exists {
		bucket = make(map[string]Provider)
		r.providers[m.Type] = bucket
	}
	if _, duplicate := bucket[m.Name]; duplicate {
		return fmt.Errorf("%w: %s (%s)", ErrProviderExists, m.Name, m.Type)
	}
	bucket[m.Name] = p
	return nil
}

// Get retrieves a provider by type and name.
func (r *Registry) Get(providerType ProviderType, name string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	bucket, ok := r.providers[providerType]
	if !ok {
		return nil, fmt.Errorf("%w: type %s", ErrProviderNotFound, providerType)
	}
	p, ok := bucket[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s (%s)", ErrProviderNotFound, name, providerType)
	}
	return p, nil
}

// List returns manifests of all providers matching the given type, or all if empty.
func (r *Registry) List(providerType ProviderType) []Manifest {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []Manifest
	if providerType != "" {
		if bucket, ok := r.providers[providerType]; ok {
			for _, p := range bucket {
				result = append(result, p.Manifest())
			}
		}
		return result
	}
	for _, bucket := range r.providers {
		for _, p := range bucket {
			result = append(result, p.Manifest())
		}
	}
	return result
}

// CheckAllHealth queries the operational health of all registered providers.
func (r *Registry) CheckAllHealth(ctx context.Context) map[string]HealthStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	report := make(map[string]HealthStatus)
	for pType, bucket := range r.providers {
		for name, p := range bucket {
			key := fmt.Sprintf("%s/%s", pType, name)
			report[key] = p.Health(ctx)
		}
	}
	return report
}

// Register registers a provider with the DefaultRegistry.
func Register(p Provider) error {
	return DefaultRegistry.Register(p)
}

// Get retrieves a provider from the DefaultRegistry.
func Get(providerType ProviderType, name string) (Provider, error) {
	return DefaultRegistry.Get(providerType, name)
}

// List lists manifests from the DefaultRegistry.
func List(providerType ProviderType) []Manifest {
	return DefaultRegistry.List(providerType)
}
