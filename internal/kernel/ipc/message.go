package ipc

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MessageType represents the classification of an Agent IPC message.
type MessageType string

const (
	MessageTypeRequest MessageType = "request"
	MessageTypeReply   MessageType = "reply"
	MessageTypeSignal  MessageType = "signal"
)

// DeliveryStatus represents the lifecycle delivery state of a mailbox message.
type DeliveryStatus string

const (
	StatusPending   DeliveryStatus = "PENDING"
	StatusDelivered DeliveryStatus = "DELIVERED"
	StatusAcked     DeliveryStatus = "ACKED"
	StatusExpired   DeliveryStatus = "EXPIRED"
)

// SignalType defines system-level lifecycle and control signals for agents.
type SignalType string

const (
	SignalStop   SignalType = "STOP"
	SignalPause  SignalType = "PAUSE"
	SignalResume SignalType = "RESUME"
	SignalWake   SignalType = "WAKE"
	SignalCustom SignalType = "CUSTOM"
)

// SignalPayload contains structured metadata for signal messages.
type SignalPayload struct {
	SignalType SignalType        `json:"signal_type"`
	Reason     string            `json:"reason,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// AgentMessage represents an immutable or state-tracked kernel IPC message.
type AgentMessage struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	Namespace     string         `json:"namespace"`
	Sender        AgentAddress   `json:"sender"`
	Receiver      AgentAddress   `json:"receiver"`
	Type          MessageType    `json:"type"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Payload       []byte         `json:"payload,omitempty"`
	Status        DeliveryStatus `json:"status"`
	CreatedAt     time.Time      `json:"created_at"`
	Deadline      time.Time      `json:"deadline"`
	AckedAt       *time.Time     `json:"acked_at,omitempty"`
	TraceID       string         `json:"trace_id,omitempty"`
	SignalType    SignalType     `json:"signal_type,omitempty"`
}

func newMessage(sender, receiver AgentAddress, msgType MessageType, payload []byte, ttl time.Duration) *AgentMessage {
	if ttl <= 0 {
		ttl = 1 * time.Minute
	}
	now := time.Now().UTC()
	return &AgentMessage{
		ID:        uuid.New().String(),
		TenantID:  sender.TenantID,
		Namespace: receiver.EffectiveNamespace(),
		Sender:    sender,
		Receiver:  receiver,
		Type:      msgType,
		Payload:   payload,
		Status:    StatusPending,
		CreatedAt: now,
		Deadline:  now.Add(ttl),
	}
}

// NewMessage constructs a general AgentMessage with default pending status and UUID.
func NewMessage(sender, receiver AgentAddress, msgType MessageType, payload []byte, ttl time.Duration) (*AgentMessage, error) {
	msg := newMessage(sender, receiver, msgType, payload, ttl)
	if err := msg.Validate(); err != nil {
		return nil, err
	}
	return msg, nil
}

// NewRequest creates a request message with an optional or generated correlation ID.
func NewRequest(sender, receiver AgentAddress, correlationID string, payload []byte, ttl time.Duration) (*AgentMessage, error) {
	if correlationID == "" {
		correlationID = uuid.New().String()
	}
	msg := newMessage(sender, receiver, MessageTypeRequest, payload, ttl)
	msg.CorrelationID = correlationID
	if err := msg.Validate(); err != nil {
		return nil, err
	}
	return msg, nil
}

// NewReply creates a reply message corresponding to a prior request correlation ID.
func NewReply(sender, receiver AgentAddress, correlationID string, payload []byte, ttl time.Duration) (*AgentMessage, error) {
	if strings.TrimSpace(correlationID) == "" {
		return nil, fmt.Errorf("%w: correlation_id is required for reply message", ErrInvalidReply)
	}
	msg := newMessage(sender, receiver, MessageTypeReply, payload, ttl)
	msg.CorrelationID = correlationID
	if err := msg.Validate(); err != nil {
		return nil, err
	}
	return msg, nil
}

// NewSignal constructs a signal message carrying standard control signals.
func NewSignal(sender, receiver AgentAddress, sigType SignalType, reason string, metadata map[string]string, ttl time.Duration) (*AgentMessage, error) {
	switch sigType {
	case SignalStop, SignalPause, SignalResume, SignalWake, SignalCustom:
	default:
		return nil, fmt.Errorf("%w: unsupported signal type %q", ErrInvalidMessage, sigType)
	}

	sigPayload := SignalPayload{
		SignalType: sigType,
		Reason:     reason,
		Metadata:   metadata,
	}
	payloadBytes, err := json.Marshal(sigPayload)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to marshal signal payload: %v", ErrInvalidMessage, err)
	}

	msg := newMessage(sender, receiver, MessageTypeSignal, payloadBytes, ttl)
	msg.SignalType = sigType
	if err := msg.Validate(); err != nil {
		return nil, err
	}
	return msg, nil
}

// Validate performs structural and semantic checks on the message.
func (m *AgentMessage) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: message is nil", ErrInvalidMessage)
	}
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("%w: message id is required", ErrInvalidMessage)
	}
	if strings.TrimSpace(m.TenantID) == "" {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidMessage)
	}
	if err := m.Sender.Validate(); err != nil {
		return fmt.Errorf("%w: invalid sender address: %v", ErrInvalidMessage, err)
	}
	if err := m.Receiver.Validate(); err != nil {
		return fmt.Errorf("%w: invalid receiver address: %v", ErrInvalidMessage, err)
	}
	if m.Sender.TenantID != m.TenantID {
		return fmt.Errorf("%w: sender tenant %q does not match message tenant %q", ErrInvalidMessage, m.Sender.TenantID, m.TenantID)
	}
	if m.Receiver.TenantID != m.TenantID {
		return fmt.Errorf("%w: receiver tenant %q does not match sender tenant %q", ErrCrossTenantDenied, m.Receiver.TenantID, m.TenantID)
	}

	switch m.Type {
	case MessageTypeRequest:
	case MessageTypeReply:
		if strings.TrimSpace(m.CorrelationID) == "" {
			return fmt.Errorf("%w: correlation_id is required for reply message", ErrInvalidReply)
		}
	case MessageTypeSignal:
		switch m.SignalType {
		case SignalStop, SignalPause, SignalResume, SignalWake, SignalCustom:
		default:
			return fmt.Errorf("%w: invalid signal_type %q", ErrInvalidMessage, m.SignalType)
		}
	default:
		return fmt.Errorf("%w: unknown message type %q", ErrInvalidMessage, m.Type)
	}

	switch m.Status {
	case StatusPending, StatusDelivered, StatusAcked, StatusExpired:
	case "":
		m.Status = StatusPending
	default:
		return fmt.Errorf("%w: invalid status %q", ErrInvalidMessage, m.Status)
	}

	if m.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at cannot be zero", ErrInvalidMessage)
	}
	if m.Deadline.IsZero() {
		return fmt.Errorf("%w: deadline cannot be zero", ErrInvalidMessage)
	}
	if m.Deadline.Before(m.CreatedAt) {
		return fmt.Errorf("%w: deadline cannot be before created_at", ErrInvalidMessage)
	}

	if m.Namespace == "" {
		m.Namespace = m.Receiver.EffectiveNamespace()
	}

	return nil
}

// IsExpired checks if the message has passed its deadline relative to the given time.
func (m *AgentMessage) IsExpired(now time.Time) bool {
	if m == nil || m.Deadline.IsZero() {
		return false
	}
	return now.After(m.Deadline)
}

// ParseSignalPayload extracts the structured SignalPayload if this message is of type Signal.
func (m *AgentMessage) ParseSignalPayload() (*SignalPayload, error) {
	if m.Type != MessageTypeSignal {
		return nil, fmt.Errorf("%w: message is not a signal", ErrInvalidMessage)
	}
	var sp SignalPayload
	if len(m.Payload) == 0 {
		return &SignalPayload{SignalType: m.SignalType}, nil
	}
	if err := json.Unmarshal(m.Payload, &sp); err != nil {
		return nil, fmt.Errorf("%w: decode signal payload: %v", ErrInvalidMessage, err)
	}
	if sp.SignalType == "" {
		sp.SignalType = m.SignalType
	}
	return &sp, nil
}

// Clone creates a deep copy of the message.
func (m *AgentMessage) Clone() *AgentMessage {
	if m == nil {
		return nil
	}
	clone := *m
	if len(m.Payload) > 0 {
		clone.Payload = make([]byte, len(m.Payload))
		copy(clone.Payload, m.Payload)
	}
	if m.AckedAt != nil {
		t := *m.AckedAt
		clone.AckedAt = &t
	}
	return &clone
}
