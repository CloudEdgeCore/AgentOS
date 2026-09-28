package supervisor

import (
	"context"
	"sync"
)

// InstanceSpawner handles the concrete activation, launch, and stopping of service instances.
type InstanceSpawner interface {
	SpawnInstance(ctx context.Context, svc *Service, inst *Instance) error
	StopInstance(ctx context.Context, svc *Service, inst *Instance) error
}

// MockSpawner is an in-memory test spawner that records spawned and stopped instances.
type MockSpawner struct {
	mu      sync.RWMutex
	Spawned map[string]*Instance
	Stopped map[string]*Instance

	OnSpawn func(ctx context.Context, svc *Service, inst *Instance) error
	OnStop  func(ctx context.Context, svc *Service, inst *Instance) error
}

// NewMockSpawner creates a new MockSpawner.
func NewMockSpawner() *MockSpawner {
	return &MockSpawner{
		Spawned: make(map[string]*Instance),
		Stopped: make(map[string]*Instance),
	}
}

// SpawnInstance records the spawned instance.
func (m *MockSpawner) SpawnInstance(ctx context.Context, svc *Service, inst *Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.OnSpawn != nil {
		if err := m.OnSpawn(ctx, svc, inst); err != nil {
			return err
		}
	}
	m.Spawned[inst.ID] = cloneInstance(inst)
	return nil
}

// StopInstance records the stopped instance.
func (m *MockSpawner) StopInstance(ctx context.Context, svc *Service, inst *Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.OnStop != nil {
		if err := m.OnStop(ctx, svc, inst); err != nil {
			return err
		}
	}
	m.Stopped[inst.ID] = cloneInstance(inst)
	return nil
}

// SpawnCount returns the total number of spawned instances recorded.
func (m *MockSpawner) SpawnCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Spawned)
}

// StopCount returns the total number of stopped instances recorded.
func (m *MockSpawner) StopCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Stopped)
}
