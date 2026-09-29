package effect

import (
	"context"
	"fmt"
	"sync"
)

// Fencer validates whether an agent attempt's fencing token is current or stale.
type Fencer interface {
	ValidateFencingToken(ctx context.Context, tenantID, agentID, runID, attemptID string, fencingToken int64) error
}

// MemoryFencer provides an in-memory fencing registry for testing and development.
type MemoryFencer struct {
	mu     sync.RWMutex
	tokens map[string]int64
}

// NewMemoryFencer creates a new in-memory fencer.
func NewMemoryFencer() *MemoryFencer {
	return &MemoryFencer{
		tokens: make(map[string]int64),
	}
}

func fenceKey(tenantID, agentID, runID string) string {
	return fmt.Sprintf("%s:%s:%s", tenantID, agentID, runID)
}

// SetActiveToken sets the currently valid active fencing token for a tenant/agent/run tuple.
func (f *MemoryFencer) SetActiveToken(tenantID, agentID, runID string, token int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[fenceKey(tenantID, agentID, runID)] = token
}

// ValidateFencingToken checks if fencingToken is at least the active token.
// If active token is set and fencingToken < activeToken, ErrEffectFenced is returned.
func (f *MemoryFencer) ValidateFencingToken(ctx context.Context, tenantID, agentID, runID, attemptID string, fencingToken int64) error {
	f.mu.RLock()
	defer f.mu.RUnlock()

	key := fenceKey(tenantID, agentID, runID)
	activeToken, exists := f.tokens[key]
	if !exists {
		// If no specific run key is found, check agent-level key
		agentKey := fmt.Sprintf("%s:%s:", tenantID, agentID)
		activeToken, exists = f.tokens[agentKey]
	}

	if exists && fencingToken < activeToken {
		return fmt.Errorf("%w: fencing token %d is superseded by active token %d (attempt %s)",
			ErrEffectFenced, fencingToken, activeToken, attemptID)
	}
	return nil
}
