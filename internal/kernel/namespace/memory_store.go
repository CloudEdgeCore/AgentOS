package namespace

import (
	"context"
	"strings"
	"sync"
	"time"
)

var _ Store = (*MemoryStore)(nil)

// MemoryStore is an in-memory, thread-safe implementation of Store.
type MemoryStore struct {
	mu         sync.RWMutex
	namespaces map[string]*Namespace
	usages     map[string]*ResourceUsage
	clock      func() time.Time
}

// NewMemoryStore creates a new in-memory namespace store with UTC clock.
func NewMemoryStore() *MemoryStore {
	return NewMemoryStoreWithClock(func() time.Time {
		return time.Now().UTC()
	})
}

// NewMemoryStoreWithClock creates an in-memory namespace store with a customized clock.
func NewMemoryStoreWithClock(clock func() time.Time) *MemoryStore {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &MemoryStore{
		namespaces: make(map[string]*Namespace),
		usages:     make(map[string]*ResourceUsage),
		clock:      clock,
	}
}

func nsKey(tenantID, name string) string {
	return strings.TrimSpace(tenantID) + "/" + strings.TrimSpace(name)
}

func cloneNamespace(ns *Namespace) *Namespace {
	if ns == nil {
		return nil
	}
	cp := *ns
	if ns.Labels != nil {
		cp.Labels = make(map[string]string, len(ns.Labels))
		for k, v := range ns.Labels {
			cp.Labels[k] = v
		}
	}
	return &cp
}

func cloneUsage(u *ResourceUsage) *ResourceUsage {
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

// ensureDefaultLocked ensures the default namespace exists for the tenant.
func (s *MemoryStore) ensureDefaultLocked(tenantID string) *Namespace {
	key := nsKey(tenantID, DefaultNamespace)
	if existing, ok := s.namespaces[key]; ok {
		return existing
	}
	def := NewDefaultNamespace(tenantID)
	def.CreatedAt = s.clock()
	def.UpdatedAt = s.clock()
	s.namespaces[key] = def
	if _, ok := s.usages[key]; !ok {
		s.usages[key] = &ResourceUsage{
			TenantID:  tenantID,
			Namespace: DefaultNamespace,
			UpdatedAt: s.clock(),
		}
	}
	return def
}

// CreateNamespace creates a new namespace.
func (s *MemoryStore) CreateNamespace(ctx context.Context, ns *Namespace) error {
	if ns == nil {
		return ErrInvalidNamespaceName
	}
	tenantID := strings.TrimSpace(ns.TenantID)
	name := strings.TrimSpace(ns.Name)
	if tenantID == "" {
		return ErrInvalidNamespaceName
	}
	if err := ValidateNamespaceName(name); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureDefaultLocked(tenantID)

	key := nsKey(tenantID, name)
	if _, exists := s.namespaces[key]; exists {
		return ErrNamespaceAlreadyExists
	}

	now := s.clock()
	created := cloneNamespace(ns)
	created.TenantID = tenantID
	created.Name = name
	if created.Phase == "" {
		created.Phase = NamespacePhaseActive
	}
	if created.CreatedAt.IsZero() {
		created.CreatedAt = now
	}
	created.UpdatedAt = now

	s.namespaces[key] = created
	if _, ok := s.usages[key]; !ok {
		s.usages[key] = &ResourceUsage{
			TenantID:  tenantID,
			Namespace: name,
			UpdatedAt: now,
		}
	}
	return nil
}

// GetNamespace retrieves a namespace by tenant and name.
func (s *MemoryStore) GetNamespace(ctx context.Context, tenantID, name string) (*Namespace, error) {
	tenantID = strings.TrimSpace(tenantID)
	name = strings.TrimSpace(name)
	if tenantID == "" || name == "" {
		return nil, ErrNamespaceNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureDefaultLocked(tenantID)

	key := nsKey(tenantID, name)
	ns, exists := s.namespaces[key]
	if !exists {
		return nil, ErrNamespaceNotFound
	}
	return cloneNamespace(ns), nil
}

// UpdateNamespace updates the metadata, labels, and quota of an existing namespace.
func (s *MemoryStore) UpdateNamespace(ctx context.Context, ns *Namespace) error {
	if ns == nil {
		return ErrNamespaceNotFound
	}
	tenantID := strings.TrimSpace(ns.TenantID)
	name := strings.TrimSpace(ns.Name)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureDefaultLocked(tenantID)

	key := nsKey(tenantID, name)
	existing, exists := s.namespaces[key]
	if !exists {
		return ErrNamespaceNotFound
	}

	now := s.clock()
	existing.DisplayName = ns.DisplayName
	existing.Description = ns.Description
	existing.Quota = ns.Quota
	existing.Phase = ns.Phase
	if existing.Phase == "" {
		existing.Phase = NamespacePhaseActive
	}
	if ns.Labels != nil {
		existing.Labels = make(map[string]string, len(ns.Labels))
		for k, v := range ns.Labels {
			existing.Labels[k] = v
		}
	}
	existing.UpdatedAt = now

	return nil
}

// DeleteNamespace removes a namespace if it is not "default" and has no active resources.
func (s *MemoryStore) DeleteNamespace(ctx context.Context, tenantID, name string) error {
	tenantID = strings.TrimSpace(tenantID)
	name = strings.TrimSpace(name)
	if name == DefaultNamespace {
		return ErrDefaultNamespaceProtected
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureDefaultLocked(tenantID)

	key := nsKey(tenantID, name)
	if _, exists := s.namespaces[key]; !exists {
		return ErrNamespaceNotFound
	}

	if usage, ok := s.usages[key]; ok {
		if usage.ActiveServices > 0 || usage.ActiveInstances > 0 || usage.ActiveTasks > 0 || usage.MailboxMessages > 0 {
			return ErrNamespaceNotEmpty
		}
	}

	delete(s.namespaces, key)
	delete(s.usages, key)
	return nil
}

// ListNamespaces lists all namespaces in the tenant.
func (s *MemoryStore) ListNamespaces(ctx context.Context, tenantID string) ([]*Namespace, error) {
	tenantID = strings.TrimSpace(tenantID)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureDefaultLocked(tenantID)

	var list []*Namespace
	prefix := tenantID + "/"
	for key, ns := range s.namespaces {
		if strings.HasPrefix(key, prefix) {
			list = append(list, cloneNamespace(ns))
		}
	}
	return list, nil
}

// GetUsage returns current resource usage for a namespace.
func (s *MemoryStore) GetUsage(ctx context.Context, tenantID, name string) (*ResourceUsage, error) {
	tenantID = strings.TrimSpace(tenantID)
	name = strings.TrimSpace(name)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureDefaultLocked(tenantID)

	key := nsKey(tenantID, name)
	usage, exists := s.usages[key]
	if !exists {
		return nil, ErrNamespaceNotFound
	}
	return cloneUsage(usage), nil
}

// RecordUsageDelta applies incremental changes to resource usage counters.
func (s *MemoryStore) RecordUsageDelta(ctx context.Context, tenantID, name string, delta ResourceUsageDelta) error {
	tenantID = strings.TrimSpace(tenantID)
	name = strings.TrimSpace(name)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureDefaultLocked(tenantID)

	key := nsKey(tenantID, name)
	usage, exists := s.usages[key]
	if !exists {
		usage = &ResourceUsage{
			TenantID:  tenantID,
			Namespace: name,
		}
		s.usages[key] = usage
	}

	usage.ActiveServices += delta.ActiveServicesDelta
	if usage.ActiveServices < 0 {
		usage.ActiveServices = 0
	}
	usage.ActiveInstances += delta.ActiveInstancesDelta
	if usage.ActiveInstances < 0 {
		usage.ActiveInstances = 0
	}
	usage.ActiveTasks += delta.ActiveTasksDelta
	if usage.ActiveTasks < 0 {
		usage.ActiveTasks = 0
	}
	usage.MailboxMessages += delta.MailboxMessagesDelta
	if usage.MailboxMessages < 0 {
		usage.MailboxMessages = 0
	}
	usage.ConsumedTokens += delta.ConsumedTokensDelta
	if usage.ConsumedTokens < 0 {
		usage.ConsumedTokens = 0
	}
	usage.ConsumedCostMicroUSD += delta.ConsumedCostMicroUSDDelta
	if usage.ConsumedCostMicroUSD < 0 {
		usage.ConsumedCostMicroUSD = 0
	}
	usage.ConsumedToolCalls += delta.ConsumedToolCallsDelta
	if usage.ConsumedToolCalls < 0 {
		usage.ConsumedToolCalls = 0
	}
	usage.ConsumedWallSeconds += delta.ConsumedWallSecondsDelta
	if usage.ConsumedWallSeconds < 0 {
		usage.ConsumedWallSeconds = 0
	}
	usage.UpdatedAt = s.clock()

	return nil
}
