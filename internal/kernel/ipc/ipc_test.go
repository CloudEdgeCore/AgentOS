package ipc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupTestService() (*Service, *DefaultPolicy, *MemoryCapabilityChecker, *MemoryFencer, *MemoryAuditor, *StandardMetrics) {
	checker := NewMemoryCapabilityChecker()
	fencer := NewMemoryFencer()
	policy := NewDefaultPolicy(checker, fencer)
	auditor := NewMemoryAuditor()
	metrics := NewStandardMetrics()
	mailbox := NewDurableMemoryMailbox()

	service := NewService(ServiceConfig{
		Mailbox: mailbox,
		Policy:  policy,
		Auditor: auditor,
		Metrics: metrics,
	})

	return service, policy, checker, fencer, auditor, metrics
}

func TestAddressParsingAndValidation(t *testing.T) {
	t.Parallel()

	// 1. Valid URIs
	addr1, err := ParseAddress("agent://tenant-1/ns-prod/worker-agent")
	if err != nil {
		t.Fatalf("unexpected error parsing URI: %v", err)
	}
	if addr1.TenantID != "tenant-1" || addr1.Namespace != "ns-prod" || addr1.AgentID != "worker-agent" || addr1.InstanceID != "" {
		t.Fatalf("unexpected parsed fields: %+v", addr1)
	}
	if addr1.String() != "agent://tenant-1/ns-prod/worker-agent" {
		t.Fatalf("unexpected String(): %s", addr1.String())
	}
	if addr1.IsInstance() {
		t.Fatalf("expected logical address, got instance")
	}

	// 2. Instance URI
	addr2, err := ParseAddress("agent://tenant-1/ns-prod/worker-agent/inst-99")
	if err != nil {
		t.Fatalf("unexpected error parsing instance URI: %v", err)
	}
	if addr2.InstanceID != "inst-99" || !addr2.IsInstance() {
		t.Fatalf("expected instance address: %+v", addr2)
	}
	if addr2.String() != "agent://tenant-1/ns-prod/worker-agent/inst-99" {
		t.Fatalf("unexpected String(): %s", addr2.String())
	}

	// 3. Logical mapping
	if addr2.Logical().String() != addr1.String() {
		t.Fatalf("expected logical %s, got %s", addr1.String(), addr2.Logical().String())
	}

	// 4. Matches
	if !addr1.Matches(addr2) || !addr2.Matches(addr1) {
		t.Fatalf("expected logical and instance to match each other")
	}

	diffInst := NewInstanceAddress("tenant-1", "ns-prod", "worker-agent", "inst-100")
	if addr2.Matches(diffInst) {
		t.Fatalf("two different non-empty instances must not match")
	}

	// 5. Slash syntax
	addrSlash, err := ParseAddress("tenant-1/ns-prod/worker-agent")
	if err != nil {
		t.Fatalf("failed parsing slash format: %v", err)
	}
	if addrSlash != addr1 {
		t.Fatalf("slash format parsed differently")
	}

	// 6. Invalid addresses
	badAddresses := []string{
		"",
		"tenant-only",
		"tenant/ns",
		"tenant/ns/agent/inst/extra",
		"agent://tenant/ns/agent with spaces",
		"agent://tenant#invalid/ns/agent",
	}
	for _, bad := range badAddresses {
		if _, err := ParseAddress(bad); err == nil {
			t.Fatalf("expected error parsing %q, got nil", bad)
		}
	}
}

func TestMessageValidation(t *testing.T) {
	t.Parallel()

	sender := NewAddress("tenant-1", "ns-a", "agent-1")
	receiver := NewAddress("tenant-1", "ns-a", "agent-2")
	diffTenantReceiver := NewAddress("tenant-2", "ns-a", "agent-2")

	// Missing tenant in cross-tenant message
	if _, err := NewMessage(sender, diffTenantReceiver, MessageTypeRequest, []byte("data"), time.Minute); !errors.Is(err, ErrCrossTenantDenied) {
		t.Fatalf("expected ErrCrossTenantDenied, got: %v", err)
	}

	// Missing correlation ID for reply
	if _, err := NewReply(sender, receiver, "", []byte("reply"), time.Minute); !errors.Is(err, ErrInvalidReply) {
		t.Fatalf("expected ErrInvalidReply, got: %v", err)
	}

	// Invalid signal type
	if _, err := NewSignal(sender, receiver, SignalType("UNKNOWN"), "test", nil, time.Minute); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("expected ErrInvalidMessage, got: %v", err)
	}
}

// Test 1: Same namespace message sending, receiving, and idempotent ACK.
func TestSameNamespaceSendReceiveAck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, checker, _, _, metrics := setupTestService()

	sender := NewAddress("tenant-1", "default", "agent-sender")
	receiver := NewAddress("tenant-1", "default", "agent-receiver")

	checker.Grant(sender, CapabilitySend)
	checker.Grant(receiver, CapabilityReceive)

	msg, err := NewMessage(sender, receiver, MessageTypeRequest, []byte("hello kernel ipc"), 5*time.Minute)
	if err != nil {
		t.Fatalf("NewMessage failed: %v", err)
	}

	// 1. Send
	if err := service.Send(ctx, msg); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// 2. Receive
	received, err := service.Receive(ctx, receiver, 10)
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 message, got %d", len(received))
	}
	if received[0].ID != msg.ID {
		t.Fatalf("expected message id %s, got %s", msg.ID, received[0].ID)
	}
	if string(received[0].Payload) != "hello kernel ipc" {
		t.Fatalf("payload mismatch: %s", string(received[0].Payload))
	}
	if received[0].Status != StatusDelivered {
		t.Fatalf("expected status DELIVERED, got %s", received[0].Status)
	}

	// 3. Second receive before ack should be empty because status is now DELIVERED
	secondReceive, err := service.Receive(ctx, receiver, 10)
	if err != nil {
		t.Fatalf("second Receive failed: %v", err)
	}
	if len(secondReceive) != 0 {
		t.Fatalf("expected 0 pending messages, got %d", len(secondReceive))
	}

	// 4. Ack
	if err := service.Ack(ctx, receiver, []string{msg.ID}); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	// 5. Idempotent Ack (should succeed without error)
	if err := service.Ack(ctx, receiver, []string{msg.ID}); err != nil {
		t.Fatalf("idempotent Ack failed: %v", err)
	}

	if metrics.SentCount(MessageTypeRequest) != 1 {
		t.Fatalf("expected 1 sent message in metrics, got %d", metrics.SentCount(MessageTypeRequest))
	}
	if metrics.ReceivedCount(MessageTypeRequest) != 1 {
		t.Fatalf("expected 1 received message in metrics, got %d", metrics.ReceivedCount(MessageTypeRequest))
	}
	if metrics.AckedCount() != 2 { // 2 acks recorded
		t.Fatalf("expected 2 ack counts in metrics, got %d", metrics.AckedCount())
	}
}

// Test 2: Cross-tenant message sending blocked (ErrCrossTenantDenied).
func TestCrossTenantDenied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, checker, _, auditor, metrics := setupTestService()

	sender := NewAddress("tenant-alpha", "default", "agent-1")
	receiver := NewAddress("tenant-beta", "default", "agent-2")

	checker.Grant(sender, CapabilitySend)
	checker.Grant(receiver, CapabilityReceive)

	// Direct attempt via message creation
	_, err := NewMessage(sender, receiver, MessageTypeRequest, []byte("cross tenant payload"), time.Minute)
	if !errors.Is(err, ErrCrossTenantDenied) {
		t.Fatalf("expected ErrCrossTenantDenied, got %v", err)
	}

	// Bypass NewMessage validation and test Policy / Service enforcement
	manualMsg := &AgentMessage{
		ID:        "msg-cross-tenant-1",
		TenantID:  "tenant-alpha",
		Namespace: "default",
		Sender:    sender,
		Receiver:  receiver,
		Type:      MessageTypeRequest,
		Payload:   []byte("cross tenant attack"),
		Status:    StatusPending,
		CreatedAt: time.Now().UTC(),
		Deadline:  time.Now().UTC().Add(time.Minute),
	}

	sendErr := service.Send(ctx, manualMsg)
	if !errors.Is(sendErr, ErrCrossTenantDenied) {
		t.Fatalf("expected ErrCrossTenantDenied from service.Send, got %v", sendErr)
	}

	if metrics.DeniedCount() == 0 {
		t.Fatalf("expected metrics.DeniedCount() > 0")
	}

	// Verify audit log has recorded the denial
	records := auditor.Records()
	found := false
	for _, r := range records {
		if r.Action == "send" && r.Result == "denied" && strings.Contains(r.Reason, "cross-tenant") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("audit log did not capture cross-tenant denial")
	}
}

// Test 3: Cross-namespace message sending blocked by default, allowed with policy rule.
func TestCrossNamespacePolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, policy, checker, _, _, _ := setupTestService()

	sender := NewAddress("tenant-1", "sales", "agent-crm")
	receiver := NewAddress("tenant-1", "billing", "agent-invoice")

	checker.Grant(sender, CapabilitySend)
	checker.Grant(receiver, CapabilityReceive)

	msg, err := NewMessage(sender, receiver, MessageTypeRequest, []byte("issue invoice"), time.Minute)
	if err != nil {
		t.Fatalf("NewMessage failed: %v", err)
	}

	// 1. By default, cross-namespace is denied
	err = service.Send(ctx, msg)
	if !errors.Is(err, ErrCrossNamespaceDenied) {
		t.Fatalf("expected ErrCrossNamespaceDenied, got %v", err)
	}

	// 2. Allow sales -> billing
	policy.AllowCrossNamespace("tenant-1", "sales", "billing")

	// 3. Send again - should succeed
	if err := service.Send(ctx, msg); err != nil {
		t.Fatalf("expected Send to succeed after rule added, got %v", err)
	}

	// 4. Receiver can read it in billing namespace
	received, err := service.Receive(ctx, receiver, 10)
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 message in billing, got %d", len(received))
	}
}

// Test 4: Capability enforcement (ipc.send / ipc.receive / ipc.signal).
func TestCapabilityEnforcement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, checker, _, _, _ := setupTestService()

	sender := NewAddress("tenant-1", "default", "sender")
	receiver := NewAddress("tenant-1", "default", "receiver")

	reqMsg, err := NewMessage(sender, receiver, MessageTypeRequest, []byte("ping"), time.Minute)
	if err != nil {
		t.Fatalf("NewMessage failed: %v", err)
	}

	// Case 1: Sender lacks ipc.send
	err = service.Send(ctx, reqMsg)
	if !errors.Is(err, ErrCapabilityDenied) || !strings.Contains(err.Error(), CapabilitySend) {
		t.Fatalf("expected ErrCapabilityDenied for ipc.send, got: %v", err)
	}

	// Grant ipc.send
	checker.Grant(sender, CapabilitySend)

	// Case 2: Receiver lacks ipc.receive
	err = service.Send(ctx, reqMsg)
	if !errors.Is(err, ErrCapabilityDenied) || !strings.Contains(err.Error(), CapabilityReceive) {
		t.Fatalf("expected ErrCapabilityDenied for ipc.receive, got: %v", err)
	}

	// Grant ipc.receive
	checker.Grant(receiver, CapabilityReceive)

	// Now request message send succeeds
	if err := service.Send(ctx, reqMsg); err != nil {
		t.Fatalf("expected send to succeed, got: %v", err)
	}

	// Case 3: Signal message requires ipc.signal for sender
	sigMsg, err := NewSignal(sender, receiver, SignalPause, "scaling", nil, time.Minute)
	if err != nil {
		t.Fatalf("NewSignal failed: %v", err)
	}

	err = service.Send(ctx, sigMsg)
	if !errors.Is(err, ErrCapabilityDenied) || !strings.Contains(err.Error(), CapabilitySignal) {
		t.Fatalf("expected ErrCapabilityDenied for ipc.signal, got: %v", err)
	}

	// Grant ipc.signal
	checker.Grant(sender, CapabilitySignal)

	// Now signal message send succeeds
	if err := service.Send(ctx, sigMsg); err != nil {
		t.Fatalf("expected signal send to succeed, got: %v", err)
	}
}

// Test 5: Fencing check (stale instance cannot send or receive).
func TestFencing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, checker, fencer, _, _ := setupTestService()

	staleSender := NewInstanceAddress("tenant-1", "default", "worker", "inst-stale-1")
	validReceiver := NewAddress("tenant-1", "default", "coordinator")

	checker.Grant(staleSender, CapabilitySend)
	checker.Grant(validReceiver, CapabilityReceive)

	// Fence the stale instance
	fencer.FenceInstance(staleSender)

	msg, err := NewMessage(staleSender, validReceiver, MessageTypeRequest, []byte("stale worker payload"), time.Minute)
	if err != nil {
		t.Fatalf("NewMessage failed: %v", err)
	}

	// Send from fenced instance must fail with ErrFenced
	err = service.Send(ctx, msg)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("expected ErrFenced on send, got: %v", err)
	}

	// Also receiving from fenced instance
	fencedReceiver := NewInstanceAddress("tenant-1", "default", "worker", "inst-fenced-recv")
	checker.Grant(fencedReceiver, CapabilityReceive)
	fencer.FenceInstance(fencedReceiver)

	_, err = service.Receive(ctx, fencedReceiver, 10)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("expected ErrFenced on receive, got: %v", err)
	}
}

// Test 6: Message expiration handling.
func TestMessageExpiration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var testTime = time.Now().UTC()
	testClock := func() time.Time {
		return testTime
	}

	checker := NewMemoryCapabilityChecker()
	fencer := NewMemoryFencer()
	policy := NewDefaultPolicy(checker, fencer)
	mailbox := NewDurableMemoryMailboxWithClock(testClock)
	metrics := NewStandardMetrics()
	service := NewService(ServiceConfig{
		Mailbox: mailbox,
		Policy:  policy,
		Metrics: metrics,
	})

	sender := NewAddress("tenant-1", "default", "sender")
	receiver := NewAddress("tenant-1", "default", "receiver")
	checker.Grant(sender, CapabilitySend)
	checker.Grant(receiver, CapabilityReceive)

	// Create message with 1 second TTL
	msg, err := NewMessage(sender, receiver, MessageTypeRequest, []byte("transient"), 1*time.Second)
	if err != nil {
		t.Fatalf("NewMessage failed: %v", err)
	}

	if err := service.Send(ctx, msg); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Advance time past deadline
	testTime = testTime.Add(2 * time.Second)

	// Receive should sweep and exclude the expired message
	received, err := service.Receive(ctx, receiver, 10)
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}
	if len(received) != 0 {
		t.Fatalf("expected 0 messages due to expiry, got %d", len(received))
	}

	// Verify stored status is now EXPIRED
	storedMsg, err := mailbox.GetMessage(ctx, msg.ID)
	if err != nil {
		t.Fatalf("GetMessage failed: %v", err)
	}
	if storedMsg.Status != StatusExpired {
		t.Fatalf("expected status EXPIRED, got %s", storedMsg.Status)
	}
}

// Test 7: Request / Reply semantics and timeout handling.
func TestRequestReply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, checker, _, _, _ := setupTestService()

	client := NewAddress("tenant-1", "default", "client-agent")
	server := NewAddress("tenant-1", "default", "server-agent")

	checker.Grant(client, CapabilitySend)
	checker.Grant(client, CapabilityReceive)
	checker.Grant(server, CapabilitySend)
	checker.Grant(server, CapabilityReceive)

	// 1. Successful Request / Reply cycle
	req, err := NewRequest(client, server, "corr-100", []byte("query 1+1"), 5*time.Second)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	replySent := make(chan struct{})
	go func() {
		// Server receives request
		for {
			select {
			case <-ctxTimeout.Done():
				return
			default:
			}
			msgs, _ := service.Receive(ctxTimeout, server, 1)
			if len(msgs) > 0 {
				receivedReq := msgs[0]
				_ = service.Ack(ctxTimeout, server, []string{receivedReq.ID})

				// Server sends reply
				reply, err := NewReply(server, client, receivedReq.CorrelationID, []byte("result: 2"), 5*time.Second)
				if err != nil {
					panic(err)
				}
				if err := service.SendReply(ctxTimeout, reply); err != nil {
					panic(err)
				}
				close(replySent)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	reply, err := service.SendRequest(ctx, req, 3*time.Second)
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}
	if string(reply.Payload) != "result: 2" {
		t.Fatalf("unexpected reply payload: %s", string(reply.Payload))
	}
	if reply.CorrelationID != "corr-100" {
		t.Fatalf("unexpected correlation id: %s", reply.CorrelationID)
	}

	<-replySent

	// 2. Timeout case
	reqTimeout, err := NewRequest(client, server, "corr-timeout", []byte("unanswered"), 5*time.Second)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}

	_, err = service.SendRequest(ctx, reqTimeout, 50*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got: %v", err)
	}
}

// Test 8: Signal delivery and structured payload parsing.
func TestSignalDeliveryAndParsing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, checker, _, _, _ := setupTestService()

	orchestrator := NewAddress("tenant-1", "default", "orchestrator")
	worker := NewInstanceAddress("tenant-1", "default", "worker", "worker-node-1")

	checker.Grant(orchestrator, CapabilitySend)
	checker.Grant(orchestrator, CapabilitySignal)
	checker.Grant(worker, CapabilityReceive)

	meta := map[string]string{
		"drain_grace_period": "30s",
		"triggered_by":       "autoscaler",
	}

	sigMsg, err := service.SendSignal(ctx, orchestrator, worker, SignalStop, "scale down event", meta)
	if err != nil {
		t.Fatalf("SendSignal failed: %v", err)
	}

	received, err := service.Receive(ctx, worker, 10)
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 signal received, got %d", len(received))
	}

	rcv := received[0]
	if rcv.Type != MessageTypeSignal {
		t.Fatalf("expected MessageTypeSignal, got %s", rcv.Type)
	}
	if rcv.SignalType != SignalStop {
		t.Fatalf("expected SignalStop, got %s", rcv.SignalType)
	}

	payload, err := rcv.ParseSignalPayload()
	if err != nil {
		t.Fatalf("ParseSignalPayload failed: %v", err)
	}
	if payload.SignalType != SignalStop || payload.Reason != "scale down event" {
		t.Fatalf("parsed payload mismatch: %+v", payload)
	}
	if payload.Metadata["drain_grace_period"] != "30s" || payload.Metadata["triggered_by"] != "autoscaler" {
		t.Fatalf("metadata mismatch: %+v", payload.Metadata)
	}

	_ = sigMsg
}

// Test 9: Audit log privacy (no payload leakage) and metrics counters.
func TestAuditPrivacyAndMetrics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, checker, _, auditor, metrics := setupTestService()

	sender := NewAddress("tenant-sec", "default", "agent-sec-1")
	receiver := NewAddress("tenant-sec", "default", "agent-sec-2")

	checker.Grant(sender, CapabilitySend)
	checker.Grant(receiver, CapabilityReceive)

	secretPayload := "SUPER_SECRET_TOKEN_DO_NOT_LOG"
	msg, err := NewMessage(sender, receiver, MessageTypeRequest, []byte(secretPayload), time.Minute)
	if err != nil {
		t.Fatalf("NewMessage failed: %v", err)
	}

	if err := service.Send(ctx, msg); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	msgs, err := service.Receive(ctx, receiver, 1)
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}
	if len(msgs) > 0 {
		_ = service.Ack(ctx, receiver, []string{msgs[0].ID})
	}

	// Verify audit logs DO NOT contain the payload
	records := auditor.Records()
	if len(records) == 0 {
		t.Fatalf("expected audit records, got none")
	}

	for _, rec := range records {
		auditJSON := fmt.Sprintf("%+v", rec)
		if strings.Contains(auditJSON, secretPayload) {
			t.Fatalf("SECURITY VIOLATION: audit log contains secret payload! Log: %s", auditJSON)
		}
	}

	if metrics.SentCount(MessageTypeRequest) != 1 {
		t.Fatalf("metrics sent mismatch")
	}
	if metrics.ReceivedCount(MessageTypeRequest) != 1 {
		t.Fatalf("metrics received mismatch")
	}
	if metrics.AckedCount() != 1 {
		t.Fatalf("metrics acked mismatch")
	}
}

// Test 10: Crash recovery simulation.
func TestMailboxCrashRecoverySimulation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mailbox := NewDurableMemoryMailbox()
	sender := NewAddress("tenant-1", "default", "sender")
	receiver := NewAddress("tenant-1", "default", "receiver")

	// Send 3 messages
	for i := 1; i <= 3; i++ {
		msg, err := NewMessage(sender, receiver, MessageTypeRequest, []byte(fmt.Sprintf("msg-%d", i)), 10*time.Minute)
		if err != nil {
			t.Fatalf("NewMessage failed: %v", err)
		}
		if err := mailbox.Send(ctx, msg); err != nil {
			t.Fatalf("Send failed: %v", err)
		}
	}

	// Receive 1 message and ack it
	firstBatch, err := mailbox.Receive(ctx, receiver, 1)
	if err != nil || len(firstBatch) != 1 {
		t.Fatalf("Receive failed: %v, len=%d", err, len(firstBatch))
	}
	if err := mailbox.Ack(ctx, receiver, []string{firstBatch[0].ID}); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}

	// Simulate crash: snapshot & reload
	recoveredMailbox, err := mailbox.SimulateCrash()
	if err != nil {
		t.Fatalf("SimulateCrash failed: %v", err)
	}

	// The remaining 2 messages should still be pending and deliverable in FIFO order
	remaining, err := recoveredMailbox.Receive(ctx, receiver, 10)
	if err != nil {
		t.Fatalf("Receive from recovered mailbox failed: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("expected 2 messages remaining after crash recovery, got %d", len(remaining))
	}
	if string(remaining[0].Payload) != "msg-2" || string(remaining[1].Payload) != "msg-3" {
		t.Fatalf("recovered messages order/payload mismatch: %s, %s", string(remaining[0].Payload), string(remaining[1].Payload))
	}

	// The first message should still be in status ACKED
	firstRecovered, err := recoveredMailbox.GetMessage(ctx, firstBatch[0].ID)
	if err != nil {
		t.Fatalf("GetMessage failed: %v", err)
	}
	if firstRecovered.Status != StatusAcked {
		t.Fatalf("expected first message to remain ACKED after crash recovery, got %s", firstRecovered.Status)
	}
}

// Test 11: High-concurrency race test.
func TestConcurrencyRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, checker, _, _, _ := setupTestService()

	sender := NewAddress("tenant-race", "default", "sender")
	receiver := NewAddress("tenant-race", "default", "receiver")

	checker.Grant(sender, CapabilitySend)
	checker.Grant(receiver, CapabilityReceive)

	const workers = 30
	const messagesPerWorker = 20
	var wg sync.WaitGroup

	// Concurrent senders
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < messagesPerWorker; i++ {
				msg, err := NewMessage(sender, receiver, MessageTypeRequest, []byte(fmt.Sprintf("w%d-m%d", workerID, i)), time.Minute)
				if err != nil {
					t.Errorf("NewMessage error: %v", err)
					return
				}
				if err := service.Send(ctx, msg); err != nil {
					t.Errorf("Send error: %v", err)
					return
				}
			}
		}(w)
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Concurrent receivers and ackers
	receivedTotal := sync.Map{}
	for r := 0; r < 5; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				msgs, err := service.Receive(ctx, receiver, 10)
				if err != nil {
					t.Errorf("Receive error: %v", err)
					return
				}
				if len(msgs) > 0 {
					ids := make([]string, 0, len(msgs))
					for _, m := range msgs {
						ids = append(ids, m.ID)
						receivedTotal.Store(m.ID, true)
					}
					_ = service.Ack(ctx, receiver, ids)
				} else {
					time.Sleep(2 * time.Millisecond)
				}

				// Count total received
				count := 0
				receivedTotal.Range(func(_, _ any) bool {
					count++
					return true
				})
				if count >= workers*messagesPerWorker {
					return
				}
			}
		}()
	}

	wg.Wait()
}
