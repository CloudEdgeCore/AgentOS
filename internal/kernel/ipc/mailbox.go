package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Mailbox defines the storage interface for kernel-level Agent IPC messages.
type Mailbox interface {
	Send(ctx context.Context, msg *AgentMessage) error
	Receive(ctx context.Context, receiver AgentAddress, limit int) ([]*AgentMessage, error)
	Ack(ctx context.Context, receiver AgentAddress, messageIDs []string) error
	GetMessage(ctx context.Context, id string) (*AgentMessage, error)
	GetByCorrelationID(ctx context.Context, tenantID, correlationID string) (*AgentMessage, error)
}

// DurableMemoryMailbox is an in-memory, thread-safe implementation of Mailbox
// that supports snapshots and crash-recovery simulation.
type DurableMemoryMailbox struct {
	mu       sync.RWMutex
	messages map[string]*AgentMessage
	order    []string
	clock    func() time.Time
}

// NewDurableMemoryMailbox constructs a new in-memory durable mailbox.
func NewDurableMemoryMailbox() *DurableMemoryMailbox {
	return NewDurableMemoryMailboxWithClock(func() time.Time {
		return time.Now().UTC()
	})
}

// NewDurableMemoryMailboxWithClock allows setting an injected clock for testing.
func NewDurableMemoryMailboxWithClock(clock func() time.Time) *DurableMemoryMailbox {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &DurableMemoryMailbox{
		messages: make(map[string]*AgentMessage),
		order:    make([]string, 0),
		clock:    clock,
	}
}

// Send stores a validated message into the mailbox with StatusPending.
func (m *DurableMemoryMailbox) Send(ctx context.Context, msg *AgentMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if msg == nil {
		return fmt.Errorf("%w: nil message", ErrInvalidMessage)
	}
	if err := msg.Validate(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.messages[msg.ID]; exists {
		return ErrDuplicateMessage
	}

	now := m.clock()
	clone := msg.Clone()
	if clone.IsExpired(now) {
		clone.Status = StatusExpired
		m.messages[clone.ID] = clone
		m.order = append(m.order, clone.ID)
		return ErrMessageExpired
	}

	if clone.Status == "" {
		clone.Status = StatusPending
	}
	m.messages[clone.ID] = clone
	m.order = append(m.order, clone.ID)
	return nil
}

// Receive retrieves pending messages intended for the specified receiver.
func (m *DurableMemoryMailbox) Receive(ctx context.Context, receiver AgentAddress, limit int) ([]*AgentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := receiver.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid receiver: %v", ErrInvalidAddress, err)
	}
	if limit <= 0 {
		limit = 20
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock()
	results := make([]*AgentMessage, 0)

	for _, id := range m.order {
		if len(results) >= limit {
			break
		}
		msg, ok := m.messages[id]
		if !ok {
			continue
		}

		if msg.TenantID != receiver.TenantID {
			continue
		}
		if !receiver.Matches(msg.Receiver) {
			continue
		}

		if msg.IsExpired(now) {
			if msg.Status != StatusExpired && msg.Status != StatusAcked {
				msg.Status = StatusExpired
			}
			continue
		}

		if msg.Status == StatusPending {
			msg.Status = StatusDelivered
			results = append(results, msg.Clone())
		}
	}

	return results, nil
}

// Ack marks messages as ACKED. Acking is idempotent.
func (m *DurableMemoryMailbox) Ack(ctx context.Context, receiver AgentAddress, messageIDs []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := receiver.Validate(); err != nil {
		return fmt.Errorf("%w: invalid receiver: %v", ErrInvalidAddress, err)
	}
	if len(messageIDs) == 0 {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock()
	for _, id := range messageIDs {
		msg, ok := m.messages[id]
		if !ok {
			return fmt.Errorf("%w: %s", ErrMessageNotFound, id)
		}
		if msg.TenantID != receiver.TenantID {
			return fmt.Errorf("%w: tenant mismatch for message %s", ErrCrossTenantDenied, id)
		}
		if !receiver.Matches(msg.Receiver) {
			return fmt.Errorf("%w: receiver does not match message recipient %s", ErrInvalidAddress, id)
		}

		if msg.Status == StatusAcked {
			continue
		}
		msg.Status = StatusAcked
		ackedAt := now
		msg.AckedAt = &ackedAt
	}

	return nil
}

// GetMessage retrieves a message by its ID.
func (m *DurableMemoryMailbox) GetMessage(ctx context.Context, id string) (*AgentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	msg, ok := m.messages[id]
	if !ok {
		return nil, ErrMessageNotFound
	}
	return msg.Clone(), nil
}

// GetByCorrelationID retrieves the latest message with the specified correlation ID.
func (m *DurableMemoryMailbox) GetByCorrelationID(ctx context.Context, tenantID, correlationID string) (*AgentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	for i := len(m.order) - 1; i >= 0; i-- {
		id := m.order[i]
		msg := m.messages[id]
		if msg.TenantID == tenantID && msg.CorrelationID == correlationID {
			return msg.Clone(), nil
		}
	}
	return nil, ErrMessageNotFound
}

// mailboxSnapshot represents serialized mailbox state for crash recovery testing.
type mailboxSnapshot struct {
	Messages []*AgentMessage `json:"messages"`
	Order    []string        `json:"order"`
}

// Snapshot serializes mailbox data into bytes.
func (m *DurableMemoryMailbox) Snapshot() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]*AgentMessage, 0, len(m.messages))
	for _, id := range m.order {
		if msg, ok := m.messages[id]; ok {
			list = append(list, msg.Clone())
		}
	}

	snap := mailboxSnapshot{
		Messages: list,
		Order:    m.order,
	}
	return json.Marshal(snap)
}

// Restore reloads mailbox data from a snapshot.
func (m *DurableMemoryMailbox) Restore(data []byte) error {
	var snap mailboxSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = make(map[string]*AgentMessage, len(snap.Messages))
	for _, msg := range snap.Messages {
		m.messages[msg.ID] = msg.Clone()
	}
	m.order = snap.Order
	return nil
}

// SimulateCrash creates a new DurableMemoryMailbox instance restoring the snapshot.
func (m *DurableMemoryMailbox) SimulateCrash() (*DurableMemoryMailbox, error) {
	snap, err := m.Snapshot()
	if err != nil {
		return nil, err
	}
	newMB := NewDurableMemoryMailboxWithClock(m.clock)
	if err := newMB.Restore(snap); err != nil {
		return nil, err
	}
	return newMB, nil
}

// PostgresMailbox implements Mailbox on top of PostgreSQL agent_ipc_messages table.
type PostgresMailbox struct {
	pool  *pgxpool.Pool
	clock func() time.Time
}

// NewPostgresMailbox creates a new PostgreSQL backed Mailbox.
func NewPostgresMailbox(pool *pgxpool.Pool) *PostgresMailbox {
	return NewPostgresMailboxWithClock(pool, func() time.Time {
		return time.Now().UTC()
	})
}

// NewPostgresMailboxWithClock allows passing a custom clock for testing.
func NewPostgresMailboxWithClock(pool *pgxpool.Pool, clock func() time.Time) *PostgresMailbox {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &PostgresMailbox{
		pool:  pool,
		clock: clock,
	}
}

// Send inserts a message into agent_ipc_messages.
func (p *PostgresMailbox) Send(ctx context.Context, msg *AgentMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if msg == nil {
		return fmt.Errorf("%w: nil message", ErrInvalidMessage)
	}
	if err := msg.Validate(); err != nil {
		return err
	}

	id, err := uuid.Parse(msg.ID)
	if err != nil {
		return fmt.Errorf("%w: invalid UUID id %q: %v", ErrInvalidMessage, msg.ID, err)
	}

	now := p.clock()
	if msg.IsExpired(now) {
		return ErrMessageExpired
	}

	query := `
		INSERT INTO agent_ipc_messages (
			id, tenant_id, namespace, sender, receiver,
			message_type, correlation_id, payload, status,
			created_at, deadline, trace_id
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11, $12
		)
	`
	status := string(StatusPending)
	var correlationID *string
	if msg.CorrelationID != "" {
		c := msg.CorrelationID
		correlationID = &c
	}
	var traceID *string
	if msg.TraceID != "" {
		t := msg.TraceID
		traceID = &t
	}

	_, err = p.pool.Exec(
		ctx,
		query,
		id,
		msg.TenantID,
		msg.Receiver.EffectiveNamespace(),
		msg.Sender.String(),
		msg.Receiver.String(),
		string(msg.Type),
		correlationID,
		msg.Payload,
		status,
		msg.CreatedAt,
		msg.Deadline,
		traceID,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicateMessage
		}
		return fmt.Errorf("postgres mailbox send: %w", err)
	}
	return nil
}

// Receive retrieves pending messages and updates their status to DELIVERED.
func (p *PostgresMailbox) Receive(ctx context.Context, receiver AgentAddress, limit int) ([]*AgentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := receiver.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid receiver: %v", ErrInvalidAddress, err)
	}
	if limit <= 0 {
		limit = 20
	}

	now := p.clock()

	// Sweep expired messages for this tenant/receiver
	sweepQuery := `
		UPDATE agent_ipc_messages
		SET status = 'EXPIRED'
		WHERE tenant_id = $1
		  AND status = 'PENDING'
		  AND deadline <= $2
	`
	_, _ = p.pool.Exec(ctx, sweepQuery, receiver.TenantID, now)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres mailbox receive begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// Receiver can match exact instance URI or logical URI if instance is unspecified
	targetMatches := []string{receiver.String()}
	if receiver.IsInstance() {
		targetMatches = append(targetMatches, receiver.Logical().String())
	}

	selectQuery := `
		SELECT
			id, tenant_id, namespace, sender, receiver,
			message_type, correlation_id, payload, status,
			created_at, deadline, acked_at, trace_id
		FROM agent_ipc_messages
		WHERE tenant_id = $1
		  AND receiver = ANY($2)
		  AND status = 'PENDING'
		  AND deadline > $3
		ORDER BY created_at ASC
		LIMIT $4
		FOR UPDATE SKIP LOCKED
	`

	rows, err := tx.Query(ctx, selectQuery, receiver.TenantID, targetMatches, now, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres mailbox receive query: %w", err)
	}
	defer rows.Close()

	var results []*AgentMessage
	var deliveredIDs []uuid.UUID

	for rows.Next() {
		var (
			rawID         uuid.UUID
			tenantID      string
			namespace     string
			rawSender     string
			rawReceiver   string
			msgType       string
			correlationID *string
			payload       []byte
			status        string
			createdAt     time.Time
			deadline      time.Time
			ackedAt       *time.Time
			traceID       *string
		)

		if err := rows.Scan(
			&rawID, &tenantID, &namespace, &rawSender, &rawReceiver,
			&msgType, &correlationID, &payload, &status,
			&createdAt, &deadline, &ackedAt, &traceID,
		); err != nil {
			return nil, fmt.Errorf("scan received message: %w", err)
		}

		senderAddr, err := ParseAddress(rawSender)
		if err != nil {
			continue
		}
		receiverAddr, err := ParseAddress(rawReceiver)
		if err != nil {
			continue
		}

		msg := &AgentMessage{
			ID:        rawID.String(),
			TenantID:  tenantID,
			Namespace: namespace,
			Sender:    senderAddr,
			Receiver:  receiverAddr,
			Type:      MessageType(msgType),
			Payload:   payload,
			Status:    StatusDelivered,
			CreatedAt: createdAt,
			Deadline:  deadline,
			AckedAt:   ackedAt,
		}
		if correlationID != nil {
			msg.CorrelationID = *correlationID
		}
		if traceID != nil {
			msg.TraceID = *traceID
		}
		if msg.Type == MessageTypeSignal {
			if sp, err := msg.ParseSignalPayload(); err == nil && sp != nil {
				msg.SignalType = sp.SignalType
			}
		}

		results = append(results, msg)
		deliveredIDs = append(deliveredIDs, rawID)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate received messages: %w", err)
	}

	if len(deliveredIDs) > 0 {
		updateQuery := `
			UPDATE agent_ipc_messages
			SET status = 'DELIVERED'
			WHERE id = ANY($1)
		`
		if _, err := tx.Exec(ctx, updateQuery, deliveredIDs); err != nil {
			return nil, fmt.Errorf("update delivered status: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit receive tx: %w", err)
	}

	return results, nil
}

// Ack updates the specified messages to StatusAcked.
func (p *PostgresMailbox) Ack(ctx context.Context, receiver AgentAddress, messageIDs []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := receiver.Validate(); err != nil {
		return fmt.Errorf("%w: invalid receiver: %v", ErrInvalidAddress, err)
	}
	if len(messageIDs) == 0 {
		return nil
	}

	uuids := make([]uuid.UUID, 0, len(messageIDs))
	for _, idStr := range messageIDs {
		u, err := uuid.Parse(strings.TrimSpace(idStr))
		if err != nil {
			return fmt.Errorf("%w: invalid message id %q", ErrInvalidMessage, idStr)
		}
		uuids = append(uuids, u)
	}

	now := p.clock()
	query := `
		UPDATE agent_ipc_messages
		SET status = 'ACKED', acked_at = $1
		WHERE tenant_id = $2
		  AND id = ANY($3)
	`
	tag, err := p.pool.Exec(ctx, query, now, receiver.TenantID, uuids)
	if err != nil {
		return fmt.Errorf("postgres mailbox ack: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMessageNotFound
	}
	return nil
}

// GetMessage retrieves a message by its UUID string.
func (p *PostgresMailbox) GetMessage(ctx context.Context, id string) (*AgentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid uuid: %v", ErrInvalidMessage, err)
	}

	query := `
		SELECT
			id, tenant_id, namespace, sender, receiver,
			message_type, correlation_id, payload, status,
			created_at, deadline, acked_at, trace_id
		FROM agent_ipc_messages
		WHERE id = $1
	`
	var (
		rawID         uuid.UUID
		tenantID      string
		namespace     string
		rawSender     string
		rawReceiver   string
		msgType       string
		correlationID *string
		payload       []byte
		status        string
		createdAt     time.Time
		deadline      time.Time
		ackedAt       *time.Time
		traceID       *string
	)

	err = p.pool.QueryRow(ctx, query, u).Scan(
		&rawID, &tenantID, &namespace, &rawSender, &rawReceiver,
		&msgType, &correlationID, &payload, &status,
		&createdAt, &deadline, &ackedAt, &traceID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, fmt.Errorf("postgres get message: %w", err)
	}

	senderAddr, err := ParseAddress(rawSender)
	if err != nil {
		return nil, err
	}
	receiverAddr, err := ParseAddress(rawReceiver)
	if err != nil {
		return nil, err
	}

	msg := &AgentMessage{
		ID:        rawID.String(),
		TenantID:  tenantID,
		Namespace: namespace,
		Sender:    senderAddr,
		Receiver:  receiverAddr,
		Type:      MessageType(msgType),
		Payload:   payload,
		Status:    DeliveryStatus(status),
		CreatedAt: createdAt,
		Deadline:  deadline,
		AckedAt:   ackedAt,
	}
	if correlationID != nil {
		msg.CorrelationID = *correlationID
	}
	if traceID != nil {
		msg.TraceID = *traceID
	}
	if msg.Type == MessageTypeSignal {
		if sp, err := msg.ParseSignalPayload(); err == nil && sp != nil {
			msg.SignalType = sp.SignalType
		}
	}
	return msg, nil
}

// GetByCorrelationID retrieves the newest message matching the correlation ID.
func (p *PostgresMailbox) GetByCorrelationID(ctx context.Context, tenantID, correlationID string) (*AgentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query := `
		SELECT
			id, tenant_id, namespace, sender, receiver,
			message_type, correlation_id, payload, status,
			created_at, deadline, acked_at, trace_id
		FROM agent_ipc_messages
		WHERE tenant_id = $1
		  AND correlation_id = $2
		ORDER BY created_at DESC
		LIMIT 1
	`
	var (
		rawID       uuid.UUID
		tID         string
		namespace   string
		rawSender   string
		rawReceiver string
		msgType     string
		corrID      *string
		payload     []byte
		status      string
		createdAt   time.Time
		deadline    time.Time
		ackedAt     *time.Time
		traceID     *string
	)

	err := p.pool.QueryRow(ctx, query, tenantID, correlationID).Scan(
		&rawID, &tID, &namespace, &rawSender, &rawReceiver,
		&msgType, &corrID, &payload, &status,
		&createdAt, &deadline, &ackedAt, &traceID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, fmt.Errorf("postgres get by correlation id: %w", err)
	}

	senderAddr, err := ParseAddress(rawSender)
	if err != nil {
		return nil, err
	}
	receiverAddr, err := ParseAddress(rawReceiver)
	if err != nil {
		return nil, err
	}

	msg := &AgentMessage{
		ID:        rawID.String(),
		TenantID:  tID,
		Namespace: namespace,
		Sender:    senderAddr,
		Receiver:  receiverAddr,
		Type:      MessageType(msgType),
		Payload:   payload,
		Status:    DeliveryStatus(status),
		CreatedAt: createdAt,
		Deadline:  deadline,
		AckedAt:   ackedAt,
	}
	if corrID != nil {
		msg.CorrelationID = *corrID
	}
	if traceID != nil {
		msg.TraceID = *traceID
	}
	return msg, nil
}
