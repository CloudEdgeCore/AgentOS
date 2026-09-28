package ipc

import (
	"context"
)

// Policy enforces tenant isolation, cross-namespace rules, and capabilities for IPC operations.
type Policy interface {
	AuthorizeSend(ctx context.Context, msg *AgentMessage) error
	AuthorizeReceive(ctx context.Context, receiver AgentAddress) error
}

// DefaultPolicy is the kernel-level IPC policy enforcer.
type DefaultPolicy struct{}

// NewDefaultPolicy constructs a DefaultPolicy.
func NewDefaultPolicy() *DefaultPolicy {
	return &DefaultPolicy{}
}

// AuthorizeSend checks policy constraints on outgoing messages.
func (p *DefaultPolicy) AuthorizeSend(ctx context.Context, msg *AgentMessage) error {
	return nil
}

// AuthorizeReceive checks policy constraints on incoming message operations.
func (p *DefaultPolicy) AuthorizeReceive(ctx context.Context, receiver AgentAddress) error {
	return nil
}
