package effect

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockProvider implements Provider for testing.
type mockProvider struct {
	name      string
	callCount atomic.Int64
	executeFn func(ctx context.Context, req *EffectRequest) ([]byte, error)
}

func (m *mockProvider) Name() string {
	return m.name
}

func (m *mockProvider) Execute(ctx context.Context, req *EffectRequest) ([]byte, error) {
	m.callCount.Add(1)
	if m.executeFn != nil {
		return m.executeFn(ctx, req)
	}
	return []byte(`{"status":"ok","transaction_id":"tx_123"}`), nil
}

func TestEffect_Execute_Success(t *testing.T) {
	store := NewMemoryStore()
	fencer := NewMemoryFencer()
	metrics := NewMemoryMetrics()
	svc := NewService(store, fencer, metrics)

	prov := &mockProvider{name: "webhook"}
	if err := svc.RegisterProvider(prov); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	req := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "order-agent",
		RunID:          "run-1",
		AttemptID:      "attempt-1",
		FencingToken:   1,
		Provider:       "webhook",
		Operation:      "POST",
		IdempotencyKey: "order-456",
		Payload:        []byte(`{"amount":100}`),
	}

	receipt, err := svc.ExecuteEffect(context.Background(), req)
	if err != nil {
		t.Fatalf("ExecuteEffect failed: %v", err)
	}

	if receipt == nil {
		t.Fatal("expected non-nil receipt")
	}
	if receipt.Status != EffectStatusCommitted {
		t.Fatalf("expected status COMMITTED, got %s", receipt.Status)
	}
	if prov.callCount.Load() != 1 {
		t.Fatalf("expected 1 provider invocation, got %d", prov.callCount.Load())
	}
	if metrics.CommittedCount != 1 {
		t.Fatalf("expected 1 committed metric, got %d", metrics.CommittedCount)
	}

	// Verify persistence in store
	rec, err := svc.GetEffect(context.Background(), "tenant-1", receipt.EffectID)
	if err != nil {
		t.Fatalf("GetEffect failed: %v", err)
	}
	if rec.Status != EffectStatusCommitted {
		t.Fatalf("stored status mismatch: %s", rec.Status)
	}
}

func TestEffect_Idempotent_Replay_ReturnsCachedReceipt(t *testing.T) {
	store := NewMemoryStore()
	fencer := NewMemoryFencer()
	metrics := NewMemoryMetrics()
	svc := NewService(store, fencer, metrics)

	prov := &mockProvider{name: "payment"}
	if err := svc.RegisterProvider(prov); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	req := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "checkout-agent",
		RunID:          "run-1",
		AttemptID:      "attempt-1",
		FencingToken:   1,
		Provider:       "payment",
		Operation:      "charge",
		IdempotencyKey: "charge-uuid-1",
		Payload:        []byte(`{"charge": 50}`),
	}

	// First execution
	receipt1, err := svc.ExecuteEffect(context.Background(), req)
	if err != nil {
		t.Fatalf("first execution failed: %v", err)
	}
	if receipt1.Status != EffectStatusCommitted {
		t.Fatalf("expected COMMITTED, got %s", receipt1.Status)
	}
	if prov.callCount.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", prov.callCount.Load())
	}

	// Second execution with same idempotency key (same attempt)
	receipt2, err := svc.ExecuteEffect(context.Background(), req)
	if err != nil {
		t.Fatalf("second execution failed: %v", err)
	}

	// Logical effect must be exactly 1!
	if prov.callCount.Load() != 1 {
		t.Fatalf("provider was re-invoked! Expected 1, got %d", prov.callCount.Load())
	}
	if receipt2.EffectID != receipt1.EffectID {
		t.Fatalf("receipt mismatch: %s != %s", receipt2.EffectID, receipt1.EffectID)
	}
	if receipt2.ResultHash != receipt1.ResultHash {
		t.Fatalf("result hash mismatch: %s != %s", receipt2.ResultHash, receipt1.ResultHash)
	}
	if metrics.IdempotentHits != 1 {
		t.Fatalf("expected 1 idempotent hit, got %d", metrics.IdempotentHits)
	}
}

func TestEffect_Takeover_CommittedEffectNotReplayed(t *testing.T) {
	store := NewMemoryStore()
	fencer := NewMemoryFencer()
	metrics := NewMemoryMetrics()
	svc := NewService(store, fencer, metrics)

	prov := &mockProvider{name: "email"}
	if err := svc.RegisterProvider(prov); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	// Attempt 1 executes effect
	fencer.SetActiveToken("tenant-1", "mail-agent", "run-1", 1)
	req1 := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "mail-agent",
		RunID:          "run-1",
		AttemptID:      "attempt-1",
		FencingToken:   1,
		Provider:       "email",
		Operation:      "send",
		IdempotencyKey: "welcome-email-user-9",
		Payload:        []byte(`{"to":"user@example.com"}`),
	}
	receipt1, err := svc.ExecuteEffect(context.Background(), req1)
	if err != nil {
		t.Fatalf("attempt 1 execute failed: %v", err)
	}
	if receipt1.Status != EffectStatusCommitted {
		t.Fatalf("expected COMMITTED, got %s", receipt1.Status)
	}
	if prov.callCount.Load() != 1 {
		t.Fatalf("expected 1 invocation, got %d", prov.callCount.Load())
	}

	// Attempt 1 crashes, Attempt 2 takes over with FencingToken = 2
	fencer.SetActiveToken("tenant-1", "mail-agent", "run-1", 2)
	req2 := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "mail-agent",
		RunID:          "run-1",
		AttemptID:      "attempt-2",
		FencingToken:   2,
		Provider:       "email",
		Operation:      "send",
		IdempotencyKey: "welcome-email-user-9",
		Payload:        []byte(`{"to":"user@example.com"}`),
	}

	receipt2, err := svc.ExecuteEffect(context.Background(), req2)
	if err != nil {
		t.Fatalf("attempt 2 takeover failed: %v", err)
	}

	// CRITICAL: takeover must NOT send duplicate email!
	if prov.callCount.Load() != 1 {
		t.Fatalf("email was duplicated upon takeover! Call count: %d", prov.callCount.Load())
	}
	if receipt2.Status != EffectStatusCommitted {
		t.Fatalf("expected COMMITTED, got %s", receipt2.Status)
	}
	if receipt2.EffectID != receipt1.EffectID {
		t.Fatalf("expected same receipt ID, got %s vs %s", receipt2.EffectID, receipt1.EffectID)
	}
}

func TestEffect_Fencing_StaleAttemptRejected(t *testing.T) {
	store := NewMemoryStore()
	fencer := NewMemoryFencer()
	metrics := NewMemoryMetrics()
	svc := NewService(store, fencer, metrics)

	prov := &mockProvider{name: "db-writer"}
	if err := svc.RegisterProvider(prov); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	// Active token is 5 (e.g. attempt 5 is active)
	fencer.SetActiveToken("tenant-1", "writer-agent", "run-1", 5)

	// Stale attempt with token 3 tries to execute an effect
	req := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "writer-agent",
		RunID:          "run-1",
		AttemptID:      "attempt-3-stale",
		FencingToken:   3,
		Provider:       "db-writer",
		Operation:      "write",
		IdempotencyKey: "db-write-100",
		Payload:        []byte(`{"row": "data"}`),
	}

	_, err := svc.ExecuteEffect(context.Background(), req)
	if err == nil {
		t.Fatal("expected fencing error, got nil")
	}
	if !errors.Is(err, ErrEffectFenced) {
		t.Fatalf("expected ErrEffectFenced, got: %v", err)
	}

	// Provider must never have been invoked
	if prov.callCount.Load() != 0 {
		t.Fatalf("expected 0 provider calls for stale attempt, got %d", prov.callCount.Load())
	}
	if metrics.FencedCount != 1 {
		t.Fatalf("expected 1 fenced metric count, got %d", metrics.FencedCount)
	}
}

func TestEffect_Unknown_NeverAutoReplays(t *testing.T) {
	store := NewMemoryStore()
	fencer := NewMemoryFencer()
	metrics := NewMemoryMetrics()
	svc := NewService(store, fencer, metrics)

	prov := &mockProvider{
		name: "external-api",
		executeFn: func(ctx context.Context, req *EffectRequest) ([]byte, error) {
			// Simulate network timeout or connection reset after sending
			return nil, MarkAmbiguous(errors.New("connection reset by peer after write"))
		},
	}
	if err := svc.RegisterProvider(prov); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	req := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "api-agent",
		RunID:          "run-1",
		AttemptID:      "attempt-1",
		FencingToken:   1,
		Provider:       "external-api",
		Operation:      "post",
		IdempotencyKey: "ext-call-88",
		Payload:        []byte(`{"data": "sensitive"}`),
	}

	// First execution encounters ambiguous error
	receipt1, err := svc.ExecuteEffect(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for ambiguous failure, got nil")
	}
	if !errors.Is(err, ErrEffectUnknown) {
		t.Fatalf("expected ErrEffectUnknown, got: %v", err)
	}
	if receipt1 == nil || receipt1.Status != EffectStatusUnknown {
		t.Fatalf("expected receipt status UNKNOWN, got %v", receipt1)
	}
	if prov.callCount.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", prov.callCount.Load())
	}

	// Second execution: takeover attempt or retry
	reqRetry := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "api-agent",
		RunID:          "run-1",
		AttemptID:      "attempt-2",
		FencingToken:   2,
		Provider:       "external-api",
		Operation:      "post",
		IdempotencyKey: "ext-call-88",
		Payload:        []byte(`{"data": "sensitive"}`),
	}

	receipt2, err := svc.ExecuteEffect(context.Background(), reqRetry)
	if err == nil {
		t.Fatal("expected error on replaying UNKNOWN effect, got nil")
	}
	if !errors.Is(err, ErrEffectUnknown) {
		t.Fatalf("expected ErrEffectUnknown on retry, got: %v", err)
	}

	// CRITICAL REQUIREMENT: UNKNOWN must NEVER be auto-replayed!
	if prov.callCount.Load() != 1 {
		t.Fatalf("provider was re-invoked on UNKNOWN effect! Call count: %d (expected 1)", prov.callCount.Load())
	}
	if receipt2.Status != EffectStatusUnknown {
		t.Fatalf("expected UNKNOWN status, got %s", receipt2.Status)
	}
}

func TestEffect_ResolveUnknownEffect(t *testing.T) {
	store := NewMemoryStore()
	fencer := NewMemoryFencer()
	metrics := NewMemoryMetrics()
	svc := NewService(store, fencer, metrics)

	prov := &mockProvider{
		name: "bank",
		executeFn: func(ctx context.Context, req *EffectRequest) ([]byte, error) {
			return nil, MarkAmbiguous(errors.New("gateway timeout 504"))
		},
	}
	if err := svc.RegisterProvider(prov); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	req := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "bank-agent",
		RunID:          "run-1",
		AttemptID:      "attempt-1",
		FencingToken:   1,
		Provider:       "bank",
		Operation:      "wire",
		IdempotencyKey: "wire-999",
		Payload:        []byte(`{"amount": 50000}`),
	}

	receipt, err := svc.ExecuteEffect(context.Background(), req)
	if !errors.Is(err, ErrEffectUnknown) {
		t.Fatalf("expected ErrEffectUnknown, got %v", err)
	}

	// Operator investigates external bank logs and verifies the transaction actually succeeded
	resolvedReceipt, err := svc.ResolveUnknownEffect(
		context.Background(),
		"tenant-1",
		receipt.EffectID,
		EffectStatusCommitted,
		[]byte(`{"bank_tx":"tx_success_confirmed"}`),
		"confirmed by bank ledger audit",
	)
	if err != nil {
		t.Fatalf("ResolveUnknownEffect failed: %v", err)
	}
	if resolvedReceipt.Status != EffectStatusCommitted {
		t.Fatalf("expected COMMITTED, got %s", resolvedReceipt.Status)
	}

	// Now a subsequent caller with the same idempotency key safely receives COMMITTED
	receiptAfterResolve, err := svc.ExecuteEffect(context.Background(), req)
	if err != nil {
		t.Fatalf("subsequent execute failed: %v", err)
	}
	if receiptAfterResolve.Status != EffectStatusCommitted {
		t.Fatalf("expected COMMITTED after resolution, got %s", receiptAfterResolve.Status)
	}
	if prov.callCount.Load() != 1 {
		t.Fatalf("provider was re-invoked after resolution! Count: %d", prov.callCount.Load())
	}
}

func TestEffect_PayloadMismatch(t *testing.T) {
	store := NewMemoryStore()
	fencer := NewMemoryFencer()
	svc := NewService(store, fencer, nil)

	prov := &mockProvider{name: "webhook"}
	svc.RegisterProvider(prov)

	req1 := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "agent-1",
		Provider:       "webhook",
		Operation:      "post",
		IdempotencyKey: "key-1",
		Payload:        []byte(`{"val": 1}`),
	}
	if _, err := svc.ExecuteEffect(context.Background(), req1); err != nil {
		t.Fatalf("req1 failed: %v", err)
	}

	// Same key, different payload
	req2 := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "agent-1",
		Provider:       "webhook",
		Operation:      "post",
		IdempotencyKey: "key-1",
		Payload:        []byte(`{"val": 2}`), // DIFFERENT PAYLOAD
	}
	_, err := svc.ExecuteEffect(context.Background(), req2)
	if err == nil {
		t.Fatal("expected ErrPayloadMismatch, got nil")
	}
	if !errors.Is(err, ErrPayloadMismatch) {
		t.Fatalf("expected ErrPayloadMismatch, got: %v", err)
	}
}

func TestEffect_DeadlineExpired(t *testing.T) {
	store := NewMemoryStore()
	svc := NewService(store, nil, nil)
	prov := &mockProvider{name: "webhook"}
	svc.RegisterProvider(prov)

	past := time.Now().UTC().Add(-1 * time.Minute)
	req := &EffectRequest{
		TenantID:       "tenant-1",
		AgentID:        "agent-1",
		Provider:       "webhook",
		Operation:      "post",
		IdempotencyKey: "key-expired",
		Payload:        []byte(`{}`),
		Deadline:       &past,
	}

	_, err := svc.ExecuteEffect(context.Background(), req)
	if !errors.Is(err, ErrEffectExpired) {
		t.Fatalf("expected ErrEffectExpired, got %v", err)
	}
}

func TestEffect_ConcurrentExecution_Serialized(t *testing.T) {
	store := NewMemoryStore()
	svc := NewService(store, nil, nil)

	prov := &mockProvider{
		name: "slow-service",
		executeFn: func(ctx context.Context, req *EffectRequest) ([]byte, error) {
			time.Sleep(20 * time.Millisecond)
			return []byte(`{"result":"processed"}`), nil
		},
	}
	svc.RegisterProvider(prov)

	var wg sync.WaitGroup
	workers := 10
	receipts := make([]*EffectReceipt, workers)
	errs := make([]error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := &EffectRequest{
				TenantID:       "tenant-1",
				AgentID:        "agent-conc",
				Provider:       "slow-service",
				Operation:      "process",
				IdempotencyKey: "conc-key-1",
				Payload:        []byte(`{"data":"identical"}`),
			}
			// Slight staggered start or simultaneous
			receipt, err := svc.ExecuteEffect(context.Background(), req)
			receipts[idx] = receipt
			errs[idx] = err
		}(i)
	}

	wg.Wait()

	// Exactly one invocation of provider
	if prov.callCount.Load() != 1 {
		t.Fatalf("expected exactly 1 provider call, got %d", prov.callCount.Load())
	}

	// At least the executing thread succeeded; any conflicting thread either received committed or in-flight error
	var successCount int
	for i := 0; i < workers; i++ {
		if errs[i] == nil && receipts[i] != nil && receipts[i].Status == EffectStatusCommitted {
			successCount++
		}
	}
	if successCount == 0 {
		t.Fatal("expected at least 1 successful execution")
	}
}
