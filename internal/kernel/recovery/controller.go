// Package recovery reconciles expired runtime leases into fenced retries.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/bian-cloud-skill/agentos/internal/kernel/workload"
	"github.com/google/uuid"
)

type Repository interface {
	ListExpiredAttempts(context.Context, time.Time, int) ([]store.RecoveryCandidate, error)
	RecoverExpiredAttempt(context.Context, store.RecoverExpiredAttemptInput) (store.RecoveryResult, error)
}

type Controller struct {
	repository Repository
	batch      int
	leaseTTL   time.Duration
	now        func() time.Time
	newID      func() uuid.UUID
}

func NewController(repository Repository, batch int, leaseTTL time.Duration) *Controller {
	return &Controller{
		repository: repository, batch: batch, leaseTTL: leaseTTL,
		now: func() time.Time { return time.Now().UTC() }, newID: newUUIDv7,
	}
}

// Reconcile recovers expired attempts. Transient transaction conflicts are
// retried with bounded backoff (ADR-002).
func (c *Controller) Reconcile(ctx context.Context) (int, error) {
	return store.RetryRetryable(ctx, func() (int, error) { return c.reconcileOnce(ctx) })
}

func (c *Controller) reconcileOnce(ctx context.Context) (int, error) {
	candidates, err := c.repository.ListExpiredAttempts(ctx, c.now(), c.batch)
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, candidate := range candidates {
		spec, err := workload.Decode(candidate.TaskSpec)
		if err != nil {
			return processed, fmt.Errorf("decode task spec for expired attempt %s: %w", candidate.AttemptID, err)
		}
		_, err = c.repository.RecoverExpiredAttempt(ctx, store.RecoverExpiredAttemptInput{
			TenantID: candidate.TenantID, AttemptID: candidate.AttemptID,
			FencingToken: candidate.FencingToken, NewAttemptID: c.newID(), NewLeaseID: c.newID(),
			LeaseTTL: c.leaseTTL, MaxAttempts: spec.RetryPolicy.EffectiveMaxAttempts(),
		})
		if errors.Is(err, store.ErrFenced) || errors.Is(err, store.ErrLeaseNotExpired) || errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

func newUUIDv7() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}
