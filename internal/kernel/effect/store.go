package effect

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Store defines persistent storage and atomic state transitions for Effect records.
type Store interface {
	GetEffect(ctx context.Context, tenantID, effectID string) (*EffectRecord, error)
	GetByIdempotencyKey(ctx context.Context, tenantID, agentID, idempotencyKey string) (*EffectRecord, error)
	CreateRecord(ctx context.Context, record *EffectRecord) error
	TransitionStatus(ctx context.Context, tenantID, effectID string, expectedStatus EffectStatus, newStatus EffectStatus, receipt *EffectReceipt) (*EffectRecord, error)
	ForceStatus(ctx context.Context, tenantID, effectID string, newStatus EffectStatus, receipt *EffectReceipt) (*EffectRecord, error)
}

// MemoryStore provides a concurrent-safe, in-memory implementation of Store.
type MemoryStore struct {
	mu           sync.RWMutex
	recordsByID  map[string]*EffectRecord // key: tenantID + ":" + effectID
	recordsByKey map[string]*EffectRecord // key: tenantID + ":" + agentID + ":" + idempotencyKey
}

// NewMemoryStore creates a new MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		recordsByID:  make(map[string]*EffectRecord),
		recordsByKey: make(map[string]*EffectRecord),
	}
}

func idKey(tenantID, effectID string) string {
	return fmt.Sprintf("%s:%s", tenantID, effectID)
}

func idempotencyKey(tenantID, agentID, key string) string {
	return fmt.Sprintf("%s:%s:%s", tenantID, agentID, key)
}

func cloneRecord(r *EffectRecord) *EffectRecord {
	if r == nil {
		return nil
	}
	cp := *r
	if r.Receipt != nil {
		rcp := *r.Receipt
		cp.Receipt = &rcp
	}
	return &cp
}

// GetEffect retrieves an effect record by tenant and effect ID.
func (s *MemoryStore) GetEffect(ctx context.Context, tenantID, effectID string) (*EffectRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.recordsByID[idKey(tenantID, effectID)]
	if !ok {
		return nil, ErrEffectNotFound
	}
	return cloneRecord(rec), nil
}

// GetByIdempotencyKey retrieves an effect record by its idempotency key.
func (s *MemoryStore) GetByIdempotencyKey(ctx context.Context, tenantID, agentID, key string) (*EffectRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.recordsByKey[idempotencyKey(tenantID, agentID, key)]
	if !ok {
		return nil, ErrEffectNotFound
	}
	return cloneRecord(rec), nil
}

// CreateRecord persists a new effect record in PENDING status.
func (s *MemoryStore) CreateRecord(ctx context.Context, record *EffectRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := idempotencyKey(record.Request.TenantID, record.Request.AgentID, record.Request.IdempotencyKey)
	if _, exists := s.recordsByKey[k]; exists {
		return ErrEffectExecuting
	}

	ik := idKey(record.Request.TenantID, record.Request.ID)
	if _, exists := s.recordsByID[ik]; exists {
		return fmt.Errorf("effect record already exists: %s", record.Request.ID)
	}

	record.CreatedAt = time.Now().UTC()
	record.UpdatedAt = record.CreatedAt
	record.Version = 1

	stored := cloneRecord(record)
	s.recordsByID[ik] = stored
	s.recordsByKey[k] = stored
	return nil
}

// TransitionStatus performs an atomic Compare-And-Swap (CAS) on the effect status.
func (s *MemoryStore) TransitionStatus(ctx context.Context, tenantID, effectID string, expectedStatus EffectStatus, newStatus EffectStatus, receipt *EffectReceipt) (*EffectRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ik := idKey(tenantID, effectID)
	stored, ok := s.recordsByID[ik]
	if !ok {
		return nil, ErrEffectNotFound
	}

	if stored.Status != expectedStatus {
		return nil, fmt.Errorf("%w: current status %s does not match expected %s",
			ErrInvalidStatusTransition, stored.Status, expectedStatus)
	}

	stored.Status = newStatus
	stored.UpdatedAt = time.Now().UTC()
	stored.Version++
	if receipt != nil {
		rcp := *receipt
		stored.Receipt = &rcp
	}

	return cloneRecord(stored), nil
}

// ForceStatus updates the status directly, used for administrative resolution of UNKNOWN state.
func (s *MemoryStore) ForceStatus(ctx context.Context, tenantID, effectID string, newStatus EffectStatus, receipt *EffectReceipt) (*EffectRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ik := idKey(tenantID, effectID)
	stored, ok := s.recordsByID[ik]
	if !ok {
		return nil, ErrEffectNotFound
	}

	stored.Status = newStatus
	stored.UpdatedAt = time.Now().UTC()
	stored.Version++
	if receipt != nil {
		rcp := *receipt
		stored.Receipt = &rcp
	}

	return cloneRecord(stored), nil
}
