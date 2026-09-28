package ipc

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Service is the unified kernel-level Agent IPC service.
type Service struct {
	mailbox Mailbox
	policy  Policy

	mu           sync.RWMutex
	replyWaiters map[string]chan *AgentMessage // correlationID -> chan
}

// ServiceConfig defines options when constructing an IPC Service.
type ServiceConfig struct {
	Mailbox Mailbox
	Policy  Policy
}

// NewService constructs an IPC Service.
func NewService(cfg ServiceConfig) *Service {
	if cfg.Mailbox == nil {
		cfg.Mailbox = NewDurableMemoryMailbox()
	}
	if cfg.Policy == nil {
		cfg.Policy = NewDefaultPolicy(nil)
	}

	return &Service{
		mailbox:      cfg.Mailbox,
		policy:       cfg.Policy,
		replyWaiters: make(map[string]chan *AgentMessage),
	}
}

func (s *Service) registerReplyWaiter(correlationID string) chan *AgentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan *AgentMessage, 1)
	s.replyWaiters[correlationID] = ch
	return ch
}

func (s *Service) unregisterReplyWaiter(correlationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.replyWaiters, correlationID)
}

func (s *Service) notifyReplyWaiter(msg *AgentMessage) {
	if msg == nil || msg.CorrelationID == "" {
		return
	}
	s.mu.RLock()
	ch, ok := s.replyWaiters[msg.CorrelationID]
	s.mu.RUnlock()
	if ok {
		select {
		case ch <- msg.Clone():
		default:
		}
	}
}

// Send validates, authorizes, and persists an IPC message.
func (s *Service) Send(ctx context.Context, msg *AgentMessage) error {
	if msg == nil {
		return fmt.Errorf("%w: nil message", ErrInvalidMessage)
	}
	if err := msg.Validate(); err != nil {
		return err
	}

	if err := s.policy.AuthorizeSend(ctx, msg); err != nil {
		return err
	}

	if err := s.mailbox.Send(ctx, msg); err != nil {
		return err
	}

	if msg.Type == MessageTypeReply {
		s.notifyReplyWaiter(msg)
	}

	return nil
}

// Receive retrieves pending messages for receiver.
func (s *Service) Receive(ctx context.Context, receiver AgentAddress, limit int) ([]*AgentMessage, error) {
	if err := receiver.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidAddress, err)
	}

	if err := s.policy.AuthorizeReceive(ctx, receiver); err != nil {
		return nil, err
	}

	return s.mailbox.Receive(ctx, receiver, limit)
}

// Ack acknowledges received messages.
func (s *Service) Ack(ctx context.Context, receiver AgentAddress, messageIDs []string) error {
	if err := receiver.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAddress, err)
	}

	if err := s.policy.AuthorizeReceive(ctx, receiver); err != nil {
		return err
	}

	return s.mailbox.Ack(ctx, receiver, messageIDs)
}

// SendRequest sends a request and synchronously awaits the matching reply or timeout.
func (s *Service) SendRequest(ctx context.Context, req *AgentMessage, timeout time.Duration) (*AgentMessage, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request message", ErrInvalidMessage)
	}
	if req.CorrelationID == "" {
		req.CorrelationID = req.ID
	}
	req.Type = MessageTypeRequest

	waiter := s.registerReplyWaiter(req.CorrelationID)
	defer s.unregisterReplyWaiter(req.CorrelationID)

	if err := s.Send(ctx, req); err != nil {
		return nil, err
	}

	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrTimeout
	case reply := <-waiter:
		return reply, nil
	}
}

// SendReply validates and sends a reply message to an existing request.
func (s *Service) SendReply(ctx context.Context, reply *AgentMessage) error {
	if reply == nil {
		return fmt.Errorf("%w: nil reply message", ErrInvalidMessage)
	}
	if reply.Type != MessageTypeReply {
		return fmt.Errorf("%w: expected reply message type, got %s", ErrInvalidReply, reply.Type)
	}
	if reply.CorrelationID == "" {
		return fmt.Errorf("%w: reply must specify correlation_id", ErrInvalidReply)
	}
	return s.Send(ctx, reply)
}

// SendSignal dispatches a lifecycle or control signal to a target agent or instance.
func (s *Service) SendSignal(ctx context.Context, sender, receiver AgentAddress, sigType SignalType, reason string, metadata map[string]string) (*AgentMessage, error) {
	msg, err := NewSignal(sender, receiver, sigType, reason, metadata, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	if err := s.Send(ctx, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
