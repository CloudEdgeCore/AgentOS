package ipc

import (
	"context"
	"fmt"
	"sync"
	"time"
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

// PeerRule defines a target pattern for allowed peer communication.
type PeerRule struct {
	Namespace string `json:"namespace,omitempty"` // Target namespace; empty means same namespace as sender, "*" means any permitted namespace
	Agent     string `json:"agent"`               // Target agent ID, or "*" for any agent in that namespace
}

// Matches checks whether the rule matches the target receiver address given sender's namespace.
func (r PeerRule) Matches(senderNS string, target AgentAddress) bool {
	targetNS := target.EffectiveNamespace()
	expectedNS := r.Namespace
	if expectedNS == "" {
		expectedNS = senderNS
	}
	if expectedNS != "*" && expectedNS != targetNS {
		return false
	}
	if r.Agent != "*" && r.Agent != target.AgentID {
		return false
	}
	return true
}

// AuthorizationDecision records the complete trace and reason of an IPC authorization evaluation.
type AuthorizationDecision struct {
	Allowed     bool      `json:"allowed"`
	ReasonCode  string    `json:"reason_code"` // "AUTH_PASSED", "CROSS_TENANT_DENIED", "CROSS_NAMESPACE_DENIED", "FENCED", "CAPABILITY_DENIED", "UNAUTHORIZED_SIGNAL", "PEER_DENIED", "RECEIVER_DENIED"
	Reason      string    `json:"reason"`
	Sender      string    `json:"sender"`
	Receiver    string    `json:"receiver"`
	TenantID    string    `json:"tenant_id"`
	TraceID     string    `json:"trace_id,omitempty"`
	EvaluatedAt time.Time `json:"evaluated_at"`
}

// PeerAuthorizer evaluates whether a sender is authorized to communicate with a receiver.
type PeerAuthorizer interface {
	AuthorizePeer(ctx context.Context, sender, receiver AgentAddress) (*AuthorizationDecision, error)
}

// MemoryPeerAuthorizer provides an in-memory implementation of sender allowlist and receiver policies.
type MemoryPeerAuthorizer struct {
	mu            sync.RWMutex
	senderRules   map[string][]PeerRule // sender.Logical().String() -> []PeerRule
	receiverRules map[string][]PeerRule // receiver.Logical().String() -> []PeerRule
	defaultDeny   bool                  // If true (default), communication without matching rules is denied
}

// NewMemoryPeerAuthorizer creates a new in-memory peer authorizer.
func NewMemoryPeerAuthorizer(defaultDeny bool) *MemoryPeerAuthorizer {
	return &MemoryPeerAuthorizer{
		senderRules:   make(map[string][]PeerRule),
		receiverRules: make(map[string][]PeerRule),
		defaultDeny:   defaultDeny,
	}
}

// AllowPeer adds an allowed peer rule to sender's outbound allowlist.
func (a *MemoryPeerAuthorizer) AllowPeer(sender AgentAddress, rule PeerRule) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := sender.Logical().String()
	a.senderRules[key] = append(a.senderRules[key], rule)
}

// SetAllowedPeers replaces sender's outbound allowlist.
func (a *MemoryPeerAuthorizer) SetAllowedPeers(sender AgentAddress, rules []PeerRule) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := sender.Logical().String()
	copied := make([]PeerRule, len(rules))
	copy(copied, rules)
	a.senderRules[key] = copied
}

// AllowReceiver adds an allowed sender rule to receiver's inbound policy.
func (a *MemoryPeerAuthorizer) AllowReceiver(receiver AgentAddress, rule PeerRule) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := receiver.Logical().String()
	a.receiverRules[key] = append(a.receiverRules[key], rule)
}

// SetReceiverPolicy replaces receiver's inbound policy rules.
func (a *MemoryPeerAuthorizer) SetReceiverPolicy(receiver AgentAddress, rules []PeerRule) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := receiver.Logical().String()
	copied := make([]PeerRule, len(rules))
	copy(copied, rules)
	a.receiverRules[key] = copied
}

// AuthorizePeer evaluates sender allowlist and receiver policy.
func (a *MemoryPeerAuthorizer) AuthorizePeer(ctx context.Context, sender, receiver AgentAddress) (*AuthorizationDecision, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	now := time.Now().UTC()
	senderKey := sender.Logical().String()
	receiverKey := receiver.Logical().String()

	// 1. Evaluate Sender Outbound Allowlist
	senderRules, hasSenderRules := a.senderRules[senderKey]
	matchedSender := false
	if hasSenderRules {
		for _, rule := range senderRules {
			if rule.Matches(sender.EffectiveNamespace(), receiver) {
				matchedSender = true
				break
			}
		}
	}

	if !matchedSender && a.defaultDeny {
		return &AuthorizationDecision{
			Allowed:     false,
			ReasonCode:  "PEER_DENIED",
			Reason:      fmt.Sprintf("sender %s has no allowed_peers rule matching receiver %s", sender.String(), receiver.String()),
			Sender:      sender.String(),
			Receiver:    receiver.String(),
			TenantID:    sender.TenantID,
			EvaluatedAt: now,
		}, fmt.Errorf("%w: sender %s not authorized to send to %s", ErrPeerDenied, sender.String(), receiver.String())
	}

	// 2. Evaluate Receiver Inbound Policy (if configured for this receiver)
	receiverRules, hasReceiverRules := a.receiverRules[receiverKey]
	if hasReceiverRules && len(receiverRules) > 0 {
		matchedReceiver := false
		for _, rule := range receiverRules {
			if rule.Matches(receiver.EffectiveNamespace(), sender) {
				matchedReceiver = true
				break
			}
		}
		if !matchedReceiver {
			return &AuthorizationDecision{
				Allowed:     false,
				ReasonCode:  "RECEIVER_DENIED",
				Reason:      fmt.Sprintf("receiver %s policy rejected sender %s", receiver.String(), sender.String()),
				Sender:      sender.String(),
				Receiver:    receiver.String(),
				TenantID:    receiver.TenantID,
				EvaluatedAt: now,
			}, fmt.Errorf("%w: receiver %s policy denied sender %s", ErrReceiverDenied, receiver.String(), sender.String())
		}
	}

	return &AuthorizationDecision{
		Allowed:     true,
		ReasonCode:  "AUTH_PASSED",
		Reason:      "peer authorization passed",
		Sender:      sender.String(),
		Receiver:    receiver.String(),
		TenantID:    sender.TenantID,
		EvaluatedAt: now,
	}, nil
}

// Policy enforces tenant isolation, cross-namespace rules, capabilities, and peer rules for IPC operations.
type Policy interface {
	AuthorizeSend(ctx context.Context, msg *AgentMessage) error
	AuthorizeReceive(ctx context.Context, receiver AgentAddress) error
}

// PolicyOption configures a DefaultPolicy.
type PolicyOption func(*DefaultPolicy)

// WithPeerAuthorizer installs a custom PeerAuthorizer.
func WithPeerAuthorizer(authorizer PeerAuthorizer) PolicyOption {
	return func(p *DefaultPolicy) {
		p.peerAuthorizer = authorizer
	}
}

// DefaultPolicy is the kernel-level IPC policy enforcer.
type DefaultPolicy struct {
	mu                  sync.RWMutex
	crossNamespaceRules map[string]bool // "<tenant>:<from>-><to>" => allowed
	capabilities        CapabilityChecker
	fencer              Fencer
	peerAuthorizer      PeerAuthorizer
}

// NewDefaultPolicy constructs a DefaultPolicy with optional capability checker, fencer, and options.
func NewDefaultPolicy(checker CapabilityChecker, fencer Fencer, opts ...PolicyOption) *DefaultPolicy {
	p := &DefaultPolicy{
		crossNamespaceRules: make(map[string]bool),
		capabilities:        checker,
		fencer:              fencer,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func ruleKey(tenantID, fromNS, toNS string) string {
	return fmt.Sprintf("%s:%s->%s", tenantID, fromNS, toNS)
}

// SetPeerAuthorizer assigns a PeerAuthorizer to the policy.
func (p *DefaultPolicy) SetPeerAuthorizer(authorizer PeerAuthorizer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peerAuthorizer = authorizer
}

// PeerAuthorizer returns the configured PeerAuthorizer, if any.
func (p *DefaultPolicy) PeerAuthorizer() PeerAuthorizer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.peerAuthorizer
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

// AllowPeer adds an allowed peer rule to sender's outbound allowlist.
// If no PeerAuthorizer is configured yet, an in-memory default-deny authorizer is automatically initialized.
func (p *DefaultPolicy) AllowPeer(sender AgentAddress, rule PeerRule) {
	p.mu.Lock()
	if p.peerAuthorizer == nil {
		p.peerAuthorizer = NewMemoryPeerAuthorizer(true)
	}
	authorizer := p.peerAuthorizer
	p.mu.Unlock()

	if mem, ok := authorizer.(*MemoryPeerAuthorizer); ok {
		mem.AllowPeer(sender, rule)
	}
}

// AllowReceiver adds an allowed sender rule to receiver's inbound policy.
// If no PeerAuthorizer is configured yet, an in-memory default-deny authorizer is automatically initialized.
func (p *DefaultPolicy) AllowReceiver(receiver AgentAddress, rule PeerRule) {
	p.mu.Lock()
	if p.peerAuthorizer == nil {
		p.peerAuthorizer = NewMemoryPeerAuthorizer(true)
	}
	authorizer := p.peerAuthorizer
	p.mu.Unlock()

	if mem, ok := authorizer.(*MemoryPeerAuthorizer); ok {
		mem.AllowReceiver(receiver, rule)
	}
}

// AuthorizeSendWithDecision validates Identity, Tenant Isolation, Namespace Boundary, Fencing, Capabilities, and Peer Policy.
func (p *DefaultPolicy) AuthorizeSendWithDecision(ctx context.Context, msg *AgentMessage) (*AuthorizationDecision, error) {
	now := time.Now().UTC()
	if msg == nil {
		return &AuthorizationDecision{
			Allowed:     false,
			ReasonCode:  "INVALID_MESSAGE",
			Reason:      "nil message",
			EvaluatedAt: now,
		}, fmt.Errorf("%w: nil message", ErrInvalidMessage)
	}

	decision := &AuthorizationDecision{
		Sender:      msg.Sender.String(),
		Receiver:    msg.Receiver.String(),
		TenantID:    msg.TenantID,
		TraceID:     msg.TraceID,
		EvaluatedAt: now,
	}

	// 1. Identity Validation
	if err := msg.Sender.Validate(); err != nil {
		decision.ReasonCode = "INVALID_SENDER"
		decision.Reason = fmt.Sprintf("invalid sender address: %v", err)
		return decision, fmt.Errorf("%w: sender: %v", ErrInvalidAddress, err)
	}
	if err := msg.Receiver.Validate(); err != nil {
		decision.ReasonCode = "INVALID_RECEIVER"
		decision.Reason = fmt.Sprintf("invalid receiver address: %v", err)
		return decision, fmt.Errorf("%w: receiver: %v", ErrInvalidAddress, err)
	}

	// 2. Strict Tenant Isolation: Cross-tenant is unconditionally forbidden
	if msg.Sender.TenantID != msg.Receiver.TenantID {
		decision.ReasonCode = "CROSS_TENANT_DENIED"
		decision.Reason = fmt.Sprintf("sender tenant %s != receiver tenant %s", msg.Sender.TenantID, msg.Receiver.TenantID)
		return decision, fmt.Errorf("%w: %s", ErrCrossTenantDenied, decision.Reason)
	}

	// 3. Namespace Boundary: Cross-namespace is default-deny unless explicitly allowed
	fromNS := msg.Sender.EffectiveNamespace()
	toNS := msg.Receiver.EffectiveNamespace()
	if fromNS != toNS {
		p.mu.RLock()
		allowed := p.crossNamespaceRules[ruleKey(msg.Sender.TenantID, fromNS, toNS)]
		p.mu.RUnlock()
		if !allowed {
			decision.ReasonCode = "CROSS_NAMESPACE_DENIED"
			decision.Reason = fmt.Sprintf("communication from %s to %s is denied by default", fromNS, toNS)
			return decision, fmt.Errorf("%w: %s", ErrCrossNamespaceDenied, decision.Reason)
		}
	}

	// 4. Fencing Verification
	if p.fencer != nil {
		if msg.Sender.IsInstance() {
			if err := p.fencer.ValidateInstance(ctx, msg.Sender); err != nil {
				decision.ReasonCode = "FENCED"
				decision.Reason = fmt.Sprintf("sender instance %s is fenced: %v", msg.Sender.String(), err)
				return decision, err
			}
		}
		if msg.Receiver.IsInstance() {
			if err := p.fencer.ValidateInstance(ctx, msg.Receiver); err != nil {
				decision.ReasonCode = "FENCED"
				decision.Reason = fmt.Sprintf("receiver instance %s is fenced: %v", msg.Receiver.String(), err)
				return decision, err
			}
		}
	}

	// 5. Capability Authorization
	if p.capabilities != nil {
		// Sender must have ipc.send
		if err := p.capabilities.CheckCapability(ctx, msg.Sender.TenantID, msg.Sender, CapabilitySend); err != nil {
			decision.ReasonCode = "CAPABILITY_DENIED"
			decision.Reason = fmt.Sprintf("sender lacks %s: %v", CapabilitySend, err)
			return decision, fmt.Errorf("%w: sender: %v", ErrCapabilityDenied, err)
		}
		// Sender of signal must have ipc.signal
		if msg.Type == MessageTypeSignal {
			if err := p.capabilities.CheckCapability(ctx, msg.Sender.TenantID, msg.Sender, CapabilitySignal); err != nil {
				decision.ReasonCode = "UNAUTHORIZED_SIGNAL"
				decision.Reason = fmt.Sprintf("signal sender lacks %s: %v", CapabilitySignal, err)
				return decision, fmt.Errorf("%w: signal sender: %w", ErrUnauthorizedSignal, err)
			}
		}
		// Receiver must have ipc.receive
		if err := p.capabilities.CheckCapability(ctx, msg.Receiver.TenantID, msg.Receiver, CapabilityReceive); err != nil {
			decision.ReasonCode = "CAPABILITY_DENIED"
			decision.Reason = fmt.Sprintf("receiver lacks %s: %v", CapabilityReceive, err)
			return decision, fmt.Errorf("%w: receiver: %v", ErrCapabilityDenied, err)
		}
	}

	// 6. Peer Policy Authorization (Sender Allowlist & Receiver Policy)
	p.mu.RLock()
	authorizer := p.peerAuthorizer
	p.mu.RUnlock()
	if authorizer != nil {
		peerDec, err := authorizer.AuthorizePeer(ctx, msg.Sender, msg.Receiver)
		if err != nil {
			if peerDec != nil {
				decision.ReasonCode = peerDec.ReasonCode
				decision.Reason = peerDec.Reason
			} else {
				decision.ReasonCode = "PEER_DENIED"
				decision.Reason = err.Error()
			}
			return decision, err
		}
	}

	// 7. All checks passed
	decision.Allowed = true
	decision.ReasonCode = "AUTH_PASSED"
	decision.Reason = "all authorization checks passed"
	return decision, nil
}

// AuthorizeSend checks tenant isolation, cross-namespace permissions, fencing, capabilities, and peer policy.
func (p *DefaultPolicy) AuthorizeSend(ctx context.Context, msg *AgentMessage) error {
	_, err := p.AuthorizeSendWithDecision(ctx, msg)
	return err
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
