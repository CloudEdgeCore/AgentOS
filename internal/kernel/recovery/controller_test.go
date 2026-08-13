package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/google/uuid"
)

type fakeRepository struct {
	candidates []store.RecoveryCandidate
	inputs     []store.RecoverExpiredAttemptInput
	err        error
}

func (f *fakeRepository) ListExpiredAttempts(context.Context, time.Time, int) ([]store.RecoveryCandidate, error) {
	return f.candidates, nil
}

func (f *fakeRepository) RecoverExpiredAttempt(_ context.Context, input store.RecoverExpiredAttemptInput) (store.RecoveryResult, error) {
	f.inputs = append(f.inputs, input)
	return store.RecoveryResult{}, f.err
}

func TestControllerUsesTaskRetryPolicy(t *testing.T) {
	repository := &fakeRepository{candidates: []store.RecoveryCandidate{{
		TenantID: "tenant-a", AttemptID: uuid.New(), FencingToken: 4,
		TaskSpec: json.RawMessage(`{"retryPolicy":{"maxAttempts":5}}`),
	}}}
	controller := NewController(repository, 10, 30*time.Second)
	processed, err := controller.Reconcile(context.Background())
	if err != nil || processed != 1 {
		t.Fatalf("Reconcile() = %d, %v", processed, err)
	}
	if len(repository.inputs) != 1 || repository.inputs[0].MaxAttempts != 5 || repository.inputs[0].FencingToken != 4 {
		t.Fatalf("unexpected recovery input: %+v", repository.inputs)
	}
}

func TestControllerIgnoresLostRecoveryRace(t *testing.T) {
	repository := &fakeRepository{
		candidates: []store.RecoveryCandidate{{TenantID: "tenant-a", AttemptID: uuid.New(), FencingToken: 1, TaskSpec: json.RawMessage(`{}`)}},
		err:        store.ErrFenced,
	}
	processed, err := NewController(repository, 10, time.Second).Reconcile(context.Background())
	if err != nil || processed != 0 {
		t.Fatalf("Reconcile() = %d, %v", processed, err)
	}
	if !errors.Is(repository.err, store.ErrFenced) {
		t.Fatal("test repository lost sentinel")
	}
}
