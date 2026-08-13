// Package outbox delivers PostgreSQL outbox events to a durable publisher.
// Delivery is at-least-once: a crash after broker acknowledgement and before
// the database acknowledgement may publish a duplicate.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

type Repository interface {
	ClaimOutbox(context.Context, store.ClaimOutboxInput) ([]store.OutboxEvent, error)
	MarkOutboxPublished(context.Context, uuid.UUID, string, int64, time.Time) error
	MarkOutboxFailed(context.Context, uuid.UUID, string, int64, string, time.Time) error
}

type Publisher interface {
	Publish(context.Context, store.OutboxEvent) error
}

type Dispatcher struct {
	repository Repository
	publisher  Publisher
	id         string
	batch      int
	lockTTL    time.Duration
	now        func() time.Time
}

func NewDispatcher(repository Repository, publisher Publisher, id string, batch int, lockTTL time.Duration) *Dispatcher {
	return &Dispatcher{
		repository: repository, publisher: publisher, id: id, batch: batch, lockTTL: lockTTL,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	events, err := d.repository.ClaimOutbox(ctx, store.ClaimOutboxInput{
		DispatcherID: d.id, Limit: d.batch, LockTTL: d.lockTTL,
	})
	if err != nil {
		return 0, err
	}
	published := 0
	var failures []error
	for _, event := range events {
		if err := d.publisher.Publish(ctx, event); err != nil {
			retryAt := d.now().Add(retryBackoff(event.PublishAttempts))
			if markErr := d.repository.MarkOutboxFailed(ctx, event.ID, d.id, event.LockFencingToken, err.Error(), retryAt); markErr != nil {
				failures = append(failures, fmt.Errorf("publish event %s: %w; mark failed: %v", event.ID, err, markErr))
			} else {
				failures = append(failures, fmt.Errorf("publish event %s: %w", event.ID, err))
			}
			continue
		}
		if err := d.repository.MarkOutboxPublished(ctx, event.ID, d.id, event.LockFencingToken, d.now()); err != nil {
			failures = append(failures, fmt.Errorf("acknowledge event %s: %w", event.ID, err))
			continue
		}
		published++
	}
	return published, errors.Join(failures...)
}

func retryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exponent := math.Min(float64(attempt-1), 8)
	return time.Duration(math.Pow(2, exponent)) * time.Second
}
