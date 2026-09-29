package effect

import (
	"context"
	"sync"
)

// Metrics records telemetry and counters for effect lifecycle events.
type Metrics interface {
	RecordExecuted(ctx context.Context, tenantID, provider, operation string)
	RecordCommitted(ctx context.Context, tenantID, provider, operation string)
	RecordFailed(ctx context.Context, tenantID, provider, operation, reason string)
	RecordUnknown(ctx context.Context, tenantID, provider, operation string)
	RecordFenced(ctx context.Context, tenantID, provider string)
	RecordIdempotentHit(ctx context.Context, tenantID, provider, operation string)
}

// NoopMetrics is a no-op implementation of Metrics.
type NoopMetrics struct{}

func (NoopMetrics) RecordExecuted(context.Context, string, string, string)       {}
func (NoopMetrics) RecordCommitted(context.Context, string, string, string)      {}
func (NoopMetrics) RecordFailed(context.Context, string, string, string, string) {}
func (NoopMetrics) RecordUnknown(context.Context, string, string, string)        {}
func (NoopMetrics) RecordFenced(context.Context, string, string)                 {}
func (NoopMetrics) RecordIdempotentHit(context.Context, string, string, string)  {}

// MemoryMetrics tracks counts in memory for test verification and monitoring.
type MemoryMetrics struct {
	mu             sync.Mutex
	ExecutedCount  int64
	CommittedCount int64
	FailedCount    int64
	UnknownCount   int64
	FencedCount    int64
	IdempotentHits int64
}

// NewMemoryMetrics creates a new MemoryMetrics instance.
func NewMemoryMetrics() *MemoryMetrics {
	return &MemoryMetrics{}
}

func (m *MemoryMetrics) RecordExecuted(ctx context.Context, tenantID, provider, operation string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ExecutedCount++
}

func (m *MemoryMetrics) RecordCommitted(ctx context.Context, tenantID, provider, operation string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CommittedCount++
}

func (m *MemoryMetrics) RecordFailed(ctx context.Context, tenantID, provider, operation, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.FailedCount++
}

func (m *MemoryMetrics) RecordUnknown(ctx context.Context, tenantID, provider, operation string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UnknownCount++
}

func (m *MemoryMetrics) RecordFenced(ctx context.Context, tenantID, provider string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.FencedCount++
}

func (m *MemoryMetrics) RecordIdempotentHit(ctx context.Context, tenantID, provider, operation string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.IdempotentHits++
}
