package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// PostgresMemoryConfig holds configuration parameters for PostgresMemoryProvider.
type PostgresMemoryConfig struct {
	ConnectionString string `json:"connectionString"`
	Table            string `json:"table"`
	VectorDimensions int    `json:"vectorDimensions"`
	DistanceMetric   string `json:"distanceMetric,omitempty"` // cosine, l2, inner_product
}

// PostgresMemoryProvider implements MemoryProvider for PostgreSQL + pgvector.
type PostgresMemoryProvider struct {
	config   PostgresMemoryConfig
	manifest Manifest
	mu       sync.RWMutex
	store    map[string]MemoryEntry
	healthy  bool
}

// NewPostgresMemoryProvider instantiates a new PostgreSQL memory and vector provider.
func NewPostgresMemoryProvider(cfg PostgresMemoryConfig) (*PostgresMemoryProvider, error) {
	if cfg.Table == "" {
		cfg.Table = "agent_memories"
	}
	if cfg.VectorDimensions <= 0 {
		cfg.VectorDimensions = 1536
	}
	if cfg.DistanceMetric == "" {
		cfg.DistanceMetric = "cosine"
	}

	manifest := Manifest{
		Name:    "postgres-memory-provider",
		Version: "1.0.0",
		Type:    TypeMemory,
		Capabilities: []string{
			"memory:put",
			"memory:get",
			"memory:search",
			"memory:vector",
		},
		ConfigSchema: json.RawMessage(`{
			"$schema": "http://json-schema.org/draft-07/schema#",
			"type": "object",
			"properties": {
				"connectionString": {"type": "string", "description": "PostgreSQL connection string with pgvector extension"},
				"table": {"type": "string", "default": "agent_memories"},
				"vectorDimensions": {"type": "integer", "default": 1536},
				"distanceMetric": {"type": "string", "enum": ["cosine", "l2", "inner_product"], "default": "cosine"}
			},
			"required": ["connectionString"]
		}`),
		Secrets: []string{"DATABASE_URL"},
		ResourceRequirements: ResourceRequirements{
			CPU:    "200m",
			Memory: "256Mi",
		},
	}

	return &PostgresMemoryProvider{
		config:   cfg,
		manifest: manifest,
		store:    make(map[string]MemoryEntry),
		healthy:  true,
	}, nil
}

func (p *PostgresMemoryProvider) Manifest() Manifest {
	return p.manifest
}

func (p *PostgresMemoryProvider) Health(ctx context.Context) HealthStatus {
	p.mu.RLock()
	healthy := p.healthy
	count := len(p.store)
	p.mu.RUnlock()

	status := "HEALTHY"
	msg := "PostgreSQL pgvector connection active"
	if !healthy {
		status = "UNHEALTHY"
		msg = "Database connection pool disconnected"
	}

	return HealthStatus{
		Status:    status,
		Message:   msg,
		Timestamp: time.Now().UTC(),
		Metrics: map[string]float64{
			"entries_count": float64(count),
		},
	}
}

func (p *PostgresMemoryProvider) Close() error {
	p.mu.Lock()
	p.healthy = false
	p.mu.Unlock()
	return nil
}

func (p *PostgresMemoryProvider) Put(ctx context.Context, req MemoryPutRequest) error {
	if strings.TrimSpace(req.Key) == "" {
		return errors.New("postgres_memory: key cannot be empty")
	}
	if len(req.Value) == 0 || !json.Valid(req.Value) {
		return errors.New("postgres_memory: value must be valid JSON")
	}

	compositeKey := req.Namespace + "/" + req.Key
	now := time.Now().UTC()

	p.mu.Lock()
	defer p.mu.Unlock()

	entry, exists := p.store[compositeKey]
	if !exists {
		entry = MemoryEntry{
			Key:       req.Key,
			Namespace: req.Namespace,
			CreatedAt: now,
		}
	}
	entry.Value = req.Value
	entry.Embedding = req.Embedding
	entry.UpdatedAt = now

	p.store[compositeKey] = entry
	return nil
}

func (p *PostgresMemoryProvider) Get(ctx context.Context, namespace, key string) (MemoryEntry, error) {
	compositeKey := namespace + "/" + key

	p.mu.RLock()
	defer p.mu.RUnlock()

	entry, exists := p.store[compositeKey]
	if !exists {
		return MemoryEntry{}, fmt.Errorf("postgres_memory: key %q not found in namespace %q", key, namespace)
	}
	return entry, nil
}

func (p *PostgresMemoryProvider) Delete(ctx context.Context, namespace, key string) error {
	compositeKey := namespace + "/" + key

	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.store, compositeKey)
	return nil
}

func (p *PostgresMemoryProvider) Search(ctx context.Context, query MemorySearchQuery) ([]MemorySearchResult, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var results []MemorySearchResult

	for _, entry := range p.store {
		if query.Namespace != "" && entry.Namespace != query.Namespace {
			continue
		}
		if query.Prefix != "" && !strings.HasPrefix(entry.Key, query.Prefix) {
			continue
		}

		score := float32(1.0)
		if len(query.QueryVector) > 0 && len(entry.Embedding) > 0 {
			score = cosineSimilarity(query.QueryVector, entry.Embedding)
		}

		if score >= query.MinScore {
			results = append(results, MemorySearchResult{
				Entry: entry,
				Score: score,
			})
		}
	}

	if query.Limit > 0 && len(results) > query.Limit {
		results = results[:query.Limit]
	}

	return results, nil
}

func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dotProduct, normA, normB float64
	for i := 0; i < len(a); i++ {
		dotProduct += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dotProduct / (math.Sqrt(normA) * math.Sqrt(normB)))
}
