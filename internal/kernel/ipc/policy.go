package ipc

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

const (
	// CapabilitySend authorizes an agent to send IPC messages.
	CapabilitySend = "ipc.send"
	// CapabilityReceive authorizes an agent to receive and ack IPC messages.
	CapabilityReceive = "ipc.receive"
	// CapabilitySignal authorizes an agent to dispatch control signals.
	CapabilitySignal = "ipc.signal"
)

// CapabilityChecker verifies whether an agent address possesses a required capability.
type CapabilityChecker interface {
	CheckCapability(ctx context.Context, tenantID string, addr AgentAddress, capability string) error
}

// MemoryCapabilityChecker implements in-memory capability authorization for tests and runtime checks.
type MemoryCapabilityChecker struct {
	mu     sync.RWMutex
	grants map[string]map[string]bool
}

// NewMemoryCapabilityChecker creates a new in-memory capability checker.
func NewMemoryCapabilityChecker() *MemoryCapabilityChecker {
	return &MemoryCapabilityChecker{
		grants: make(map[string]map[string]bool),
	}
}

// Grant assigns a capability to an agent address.
// If addr has no instance, the capability is granted to the logical agent (all instances).
func (c *MemoryCapabilityChecker) Grant(addr AgentAddress, capability string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := addr.String()
	if c.grants[key] == nil {
		c.grants[key] = make(map[string]bool)
	}
	c.grants[key][capability] = true

	// Also record for logical address if an instance address was provided
	if addr.IsInstance() {
		logicalKey := addr.Logical().String()
		if c.grants[logicalKey] == nil {
			c.grants[logicalKey] = make(map[string]bool)
		}
		c.grants[logicalKey][capability] = true
	}
}

// Revoke removes a capability grant.
func (c *MemoryCapabilityChecker) Revoke(addr AgentAddress, capability string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := addr.String()
	if c.grants[key] != nil {
		delete(c.grants[key], capability)
	}
	if addr.IsInstance() {
		logicalKey := addr.Logical().String()
		if c.grants[logicalKey] != nil {
			delete(c.grants[logicalKey], capability)
		}
	}
}

// CheckCapability tests if the address has the requested capability.
func (c *MemoryCapabilityChecker) CheckCapability(ctx context.Context, tenantID string, addr AgentAddress, capability string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Check exact address (with instance if present)
	if caps, ok := c.grants[addr.String()]; ok && caps[capability] {
		return nil
	}
	// Check logical address
	if caps, ok := c.grants[addr.Logical().String()]; ok && caps[capability] {
		return nil
	}

	return fmt.Errorf("%w: agent %s lacks %s", ErrCapabilityDenied, addr.String(), capability)
}

// Fencer validates whether an agent instance is active or has been fenced.
type Fencer interface {
	ValidateInstance(ctx context.Context, addr AgentAddress) error
}

// MemoryFencer provides an in-memory fencer registry for testing.
type MemoryFencer struct {
	mu     sync.RWMutex
	fenced map[string]bool
}

// NewMemoryFencer creates a new MemoryFencer.
func NewMemoryFencer() *MemoryFencer {
	return &MemoryFencer{
		fenced: make(map[string]bool),
	}
}

// FenceInstance marks an agent instance as fenced.
func (f *MemoryFencer) FenceInstance(addr AgentAddress) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fenced[addr.String()] = true
}

// UnfenceInstance removes the fenced flag from an instance.
func (f *MemoryFencer) UnfenceInstance(addr AgentAddress) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fenced, addr.String())
}

// ValidateInstance returns ErrFenced if the instance address has been fenced.
func (f *MemoryFencer) ValidateInstance(ctx context.Context, addr AgentAddress) error {
	if !addr.IsInstance() {
		return nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.fenced[addr.String()] {
		return ErrFenced
	}
	return nil
}

// Policy enforces tenant isolation, cross-namespace rules, and capabilities for IPC operations.
type Policy interface {
	AuthorizeSend(ctx context.Context, msg *AgentMessage) error
	AuthorizeReceive(ctx context.Context, receiver AgentAddress) error
}

// DefaultPolicy is the kernel-level IPC policy enforcer.
type DefaultPolicy struct {
	mu                  sync.RWMutex
	crossNamespaceRules map[string]bool // "<tenant>:<from>-><to>" => allowed
	capabilities        CapabilityChecker
	fencer              Fencer
}

// NewDefaultPolicy constructs a DefaultPolicy with optional capability checker and fencer.
func NewDefaultPolicy(checker CapabilityChecker, fencer Fencer) *DefaultPolicy {
	return &DefaultPolicy{
		crossNamespaceRules: make(map[string]bool),
		capabilities:        checker,
		fencer:              fencer,
	}
}

func ruleKey(tenantID, fromNS, toNS string) string {
	return fmt.Sprintf("%s:%s->%s", tenantID, fromNS, toNS)
}

// AllowCrossNamespace permits communication from one namespace to another for a tenant.
func (p *DefaultPolicy) AllowCrossNamespace(tenantID, fromNS, toNS string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.crossNamespaceRules[ruleKey(tenantID, fromNS, toNS)] = true
}

// DisallowCrossNamespace revokes cross-namespace permission.
func (p *DefaultPolicy) DisallowCrossNamespace(tenantID, fromNS, toNS string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.crossNamespaceRules, ruleKey(tenantID, fromNS, toNS))
}

// AuthorizeSend checks tenant isolation, cross-namespace permissions, fencing, and capabilities.
func (p *DefaultPolicy) AuthorizeSend(ctx context.Context, msg *AgentMessage) error {
	if msg == nil {
		return fmt.Errorf("%w: nil message", ErrInvalidMessage)
	}

	// 1. Strict Tenant Isolation: Cross-tenant is unconditionally forbidden
	if msg.Sender.TenantID != msg.Receiver.TenantID {
		return fmt.Errorf("%w: sender tenant %s != receiver tenant %s", ErrCrossTenantDenied, msg.Sender.TenantID, msg.Receiver.TenantID)
	}

	// 2. Namespace Boundary: Cross-namespace is default-deny unless explicitly allowed
	fromNS := msg.Sender.EffectiveNamespace()
	toNS := msg.Receiver.EffectiveNamespace()
	if fromNS != toNS {
		p.mu.RLock()
		allowed := p.crossNamespaceRules[ruleKey(msg.Sender.TenantID, fromNS, toNS)]
		p.mu.RUnlock()
		if !allowed {
			return fmt.Errorf("%w: communication from %s to %s is denied by default", ErrCrossNamespaceDenied, fromNS, toNS)
		}
	}

	// 3. Fencing Verification
	if p.fencer != nil {
		if msg.Sender.IsInstance() {
			if err := p.fencer.ValidateInstance(ctx, msg.Sender); err != nil {
				return err
			}
		}
		if msg.Receiver.IsInstance() {
			if err := p.fencer.ValidateInstance(ctx, msg.Receiver); err != nil {
				return err
			}
		}
	}

	// 4. Capability Authorization
	if p.capabilities != nil {
		// Sender must have ipc.send
		if err := p.capabilities.CheckCapability(ctx, msg.Sender.TenantID, msg.Sender, CapabilitySend); err != nil {
			return fmt.Errorf("%w: sender: %v", ErrCapabilityDenied, err)
		}
		// Sender of signal must have ipc.signal
		if msg.Type == MessageTypeSignal {
			if err := p.capabilities.CheckCapability(ctx, msg.Sender.TenantID, msg.Sender, CapabilitySignal); err != nil {
				return fmt.Errorf("%w: signal sender: %v", ErrCapabilityDenied, err)
			}
		}
		// Receiver must have ipc.receive
		if err := p.capabilities.CheckCapability(ctx, msg.Receiver.TenantID, msg.Receiver, CapabilityReceive); err != nil {
			return fmt.Errorf("%w: receiver: %v", ErrCapabilityDenied, err)
		}
	}

	return nil
}

// AuthorizeReceive verifies fencing and ipc.receive capability for a receiving agent.
func (p *DefaultPolicy) AuthorizeReceive(ctx context.Context, receiver AgentAddress) error {
	if p.fencer != nil && receiver.IsInstance() {
		if err := p.fencer.ValidateInstance(ctx, receiver); err != nil {
			return err
		}
	}

	if p.capabilities != nil {
		if err := p.capabilities.CheckCapability(ctx, receiver.TenantID, receiver, CapabilityReceive); err != nil {
			return fmt.Errorf("%w: receiver: %v", ErrCapabilityDenied, err)
		}
	}

	return nil
}
