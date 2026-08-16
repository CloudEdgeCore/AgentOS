package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrMemoryNotFound reports a memory record that does not exist in the
// tenant's scope.
var ErrMemoryNotFound = errors.New("memory record not found")

// ErrMemoryTooLarge reports content that exceeds the canonical store limit.
var ErrMemoryTooLarge = errors.New("memory content exceeds 256 KiB")

// ErrMemorySearchRequired reports a search with neither a keyword query nor
// an embedding vector.
var ErrMemorySearchRequired = errors.New("memory search requires a keyword query or an embedding")

// MemorySensitivities are the v0.1 sensitivity tiers; retrieval filters on
// them before scoring (ADR-009: the ACL check runs before retrieval).
var MemorySensitivities = map[string]struct{}{
	"internal": {}, "confidential": {}, "restricted": {},
}

// MemoryRecord is one canonical memory entry: identity, provenance, ACL,
// tenant, sensitivity, version, retention and tombstone state. Content lives
// in PostgreSQL for v0.1 (FTS + pgvector share the transaction system); large
// payloads move to the object store behind the same interface later.
type MemoryRecord struct {
	ID                uuid.UUID
	TenantID          string
	Namespace         string
	Key               string
	ContentType       string
	Content           string
	Embedding         []float32
	EmbeddingProvider string
	Sensitivity       string
	SourceTaskID      *uuid.UUID
	SourceRunID       *uuid.UUID
	SourceAttemptID   *uuid.UUID
	Provenance        json.RawMessage
	RetentionUntil    *time.Time
	TombstoneAt       *time.Time
	SupersededBy      *uuid.UUID
	ResourceVersion   int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ContentHash returns the SHA-256 of the canonical memory document; a
// PutMemory with the same (tenant, namespace, key) and an identical document
// replays idempotently instead of creating a new version. Nil provenance is
// canonicalized to {} and retention times to UTC so stored and submitted
// documents hash identically.
func (r MemoryRecord) ContentHash() ([sha256.Size]byte, error) {
	provenance := r.Provenance
	if len(provenance) == 0 {
		provenance = json.RawMessage(`{}`)
	}
	var retentionUTC *time.Time
	if r.RetentionUntil != nil {
		value := r.RetentionUntil.UTC()
		retentionUTC = &value
	}
	encoded, err := json.Marshal(struct {
		Namespace    string          `json:"namespace"`
		Key          string          `json:"key"`
		ContentType  string          `json:"contentType"`
		Content      string          `json:"content"`
		Embedding    []float32       `json:"embedding"`
		Sensitivity  string          `json:"sensitivity"`
		Provenance   json.RawMessage `json:"provenance"`
		RetentionUTC *time.Time      `json:"retentionUntil,omitempty"`
	}{r.Namespace, r.Key, r.ContentType, r.Content, r.Embedding, r.Sensitivity, provenance, retentionUTC})
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode memory document: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

type PutMemoryInput struct {
	TenantID          string
	Namespace         string
	Key               string
	ContentType       string
	Content           string
	Embedding         []float32
	EmbeddingProvider string
	Sensitivity       string
	SourceTaskID      *uuid.UUID
	SourceRunID       *uuid.UUID
	SourceAttemptID   *uuid.UUID
	Provenance        json.RawMessage
	RetentionUntil    *time.Time
}

const (
	// MemoryContentLimit is the canonical store's per-record content cap.
	MemoryContentLimit = 256 << 10
	// MemoryEmbeddingDimension is the v0.1 vector space dimension.
	MemoryEmbeddingDimension = 384
)

func (in PutMemoryInput) Validate() error {
	if strings.TrimSpace(in.TenantID) == "" || strings.TrimSpace(in.Namespace) == "" || strings.TrimSpace(in.Key) == "" {
		return fmt.Errorf("tenant, namespace, and key are required")
	}
	if strings.TrimSpace(in.ContentType) == "" || len(in.ContentType) > 255 {
		return fmt.Errorf("content type is required and must not exceed 255 bytes")
	}
	if strings.TrimSpace(in.Content) == "" {
		return fmt.Errorf("content is required")
	}
	if len(in.Content) > MemoryContentLimit {
		return ErrMemoryTooLarge
	}
	if len(in.Embedding) != MemoryEmbeddingDimension {
		return fmt.Errorf("embedding must have dimension %d", MemoryEmbeddingDimension)
	}
	if strings.TrimSpace(in.EmbeddingProvider) == "" || len(in.EmbeddingProvider) > 255 {
		return fmt.Errorf("embedding provider is required and must not exceed 255 bytes")
	}
	if _, ok := MemorySensitivities[in.Sensitivity]; !ok {
		return fmt.Errorf("sensitivity must be one of internal, confidential, restricted")
	}
	if len(in.Provenance) > 0 && !json.Valid(in.Provenance) {
		return fmt.Errorf("provenance must be a JSON object")
	}
	// A retention deadline already in the past is legal: the record is
	// immediately excluded from retrieval.
	return nil
}

type SearchMemoryInput struct {
	TenantID    string
	Query       string
	Embedding   []float32
	Namespace   string
	Sensitivity string
	Limit       int
}

func (in SearchMemoryInput) Validate() error {
	if strings.TrimSpace(in.TenantID) == "" {
		return fmt.Errorf("tenant is required")
	}
	if strings.TrimSpace(in.Query) == "" && len(in.Embedding) == 0 {
		return ErrMemorySearchRequired
	}
	if len(in.Embedding) > 0 && len(in.Embedding) != MemoryEmbeddingDimension {
		return fmt.Errorf("embedding must have dimension %d", MemoryEmbeddingDimension)
	}
	if in.Sensitivity != "" {
		if _, ok := MemorySensitivities[in.Sensitivity]; !ok {
			return fmt.Errorf("sensitivity must be one of internal, confidential, restricted")
		}
	}
	if in.Limit <= 0 || in.Limit > 100 {
		return fmt.Errorf("limit must be between 1 and 100")
	}
	return nil
}

type MemoryStore interface {
	// PutMemory inserts or updates the record with the given stable
	// (tenant, namespace, key). An identical document replays idempotently
	// (Existing=true, no version bump); a different document is a new
	// version (correction) and resurrects a tombstoned record.
	PutMemory(context.Context, PutMemoryInput) (MemoryRecord, bool, error)
	// GetMemory returns the record (including tombstone state) or
	// ErrMemoryNotFound.
	GetMemory(context.Context, string, uuid.UUID) (MemoryRecord, error)
	// SearchMemory returns live (non-tombstoned, unexpired) records scored
	// by keyword FTS/trigram similarity and optional vector cosine distance,
	// tenant- and sensitivity-filtered before scoring.
	SearchMemory(context.Context, SearchMemoryInput) ([]MemoryRecord, error)
	// TombstoneMemory soft-deletes a record (ADR-009 deletion intent), CAS on
	// resource version.
	TombstoneMemory(context.Context, string, uuid.UUID, int64) (MemoryRecord, error)
}
