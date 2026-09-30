package provider

import (
	"context"
	"encoding/json"
	"time"
)

// MemoryEntry represents a persistent key-value memory item with optional vector embedding.
type MemoryEntry struct {
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value"`
	Embedding  []float32       `json:"embedding,omitempty"`
	Namespace  string          `json:"namespace,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

// MemoryPutRequest stores or updates a memory entry.
type MemoryPutRequest struct {
	Namespace string          `json:"namespace,omitempty"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Embedding []float32       `json:"embedding,omitempty"`
	TTL       time.Duration   `json:"ttl,omitempty"`
}

// MemorySearchQuery queries semantic memory via vector similarity or prefix matching.
type MemorySearchQuery struct {
	Namespace      string    `json:"namespace,omitempty"`
	QueryVector    []float32 `json:"queryVector,omitempty"`
	Prefix         string    `json:"prefix,omitempty"`
	Limit          int       `json:"limit,omitempty"`
	MinScore       float32   `json:"minScore,omitempty"`
}

// MemorySearchResult returns a matching entry with its similarity score.
type MemorySearchResult struct {
	Entry MemoryEntry `json:"entry"`
	Score float32     `json:"score"`
}

// MemoryProvider supplies persistent and vector memory for agents.
type MemoryProvider interface {
	Provider
	Put(ctx context.Context, req MemoryPutRequest) error
	Get(ctx context.Context, namespace, key string) (MemoryEntry, error)
	Delete(ctx context.Context, namespace, key string) error
	Search(ctx context.Context, query MemorySearchQuery) ([]MemorySearchResult, error)
}
