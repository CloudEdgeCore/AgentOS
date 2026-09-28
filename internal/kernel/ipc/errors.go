package ipc

import "errors"

var (
	// ErrInvalidAddress is returned when an address fails syntactic or semantic validation.
	ErrInvalidAddress = errors.New("ipc: invalid agent address")

	// ErrInvalidMessage is returned when a message is missing required fields or has invalid attributes.
	ErrInvalidMessage = errors.New("ipc: invalid agent message")

	// ErrCrossTenantDenied is returned when an IPC attempt crosses tenant boundaries.
	ErrCrossTenantDenied = errors.New("ipc: cross-tenant communication denied")

	// ErrCrossNamespaceDenied is returned when cross-namespace communication is not permitted by policy.
	ErrCrossNamespaceDenied = errors.New("ipc: cross-namespace communication denied by policy")

	// ErrCapabilityDenied is returned when an agent lacks required IPC capabilities (ipc.send, ipc.receive, ipc.signal).
	ErrCapabilityDenied = errors.New("ipc: capability denied")

	// ErrFenced is returned when an agent instance is fenced or has an invalid/stale fencing token.
	ErrFenced = errors.New("ipc: agent instance is fenced")

	// ErrMessageNotFound is returned when a requested message cannot be found in the mailbox.
	ErrMessageNotFound = errors.New("ipc: message not found")

	// ErrMessageExpired is returned when a message has exceeded its deadline.
	ErrMessageExpired = errors.New("ipc: message deadline expired")

	// ErrDuplicateMessage is returned when a message with the same ID already exists in the mailbox.
	ErrDuplicateMessage = errors.New("ipc: duplicate message id")

	// ErrTimeout is returned when a request-reply pattern times out waiting for a reply.
	ErrTimeout = errors.New("ipc: request timed out waiting for reply")

	// ErrInvalidReply is returned when a reply is sent without a matching request or correlation ID.
	ErrInvalidReply = errors.New("ipc: invalid reply or missing correlation id")
)
