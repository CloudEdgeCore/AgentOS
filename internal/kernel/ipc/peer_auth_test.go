package ipc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPeerAuthorization_DefaultDeny(t *testing.T) {
	t.Parallel()

	service, policy, checker, _, auditor, metrics := setupTestService()
	// Install strict peer authorizer (default deny)
	authorizer := NewMemoryPeerAuthorizer(true)
	policy.SetPeerAuthorizer(authorizer)

	sender, _ := ParseAddress("agent://tenant-1/ns-prod/agent-a")
	receiver, _ := ParseAddress("agent://tenant-1/ns-prod/agent-b")

	checker.Grant(sender, CapabilitySend)
	checker.Grant(receiver, CapabilityReceive)

	ctx := context.Background()
	msg, err := NewMessage(sender, receiver, MessageTypeRequest, []byte(`{"ping":true}`), time.Minute)
	if err != nil {
		t.Fatalf("unexpected NewMessage error: %v", err)
	}

	// 1. Without allowed_peers, default deny must block transmission
	err = service.Send(ctx, msg)
	if !errors.Is(err, ErrPeerDenied) {
		t.Fatalf("expected ErrPeerDenied under default-deny, got: %v", err)
	}

	// 2. Metrics check: denied count must increment
	if metrics.DeniedCount() != 1 {
		t.Fatalf("expected 1 denied metric count, got %d", metrics.DeniedCount())
	}

	// 3. Audit check: record must capture PEER_DENIED
	records := auditor.Records()
	if len(records) == 0 {
		t.Fatalf("expected audit record logged for denied send")
	}
	lastRecord := records[len(records)-1]
	if lastRecord.Result != "denied" || lastRecord.ReasonCode != "PEER_DENIED" {
		t.Fatalf("expected audit result 'denied' and code 'PEER_DENIED', got: %+v", lastRecord)
	}
}

func TestPeerAuthorization_AllowedPeerAndDeniedPeer(t *testing.T) {
	t.Parallel()

	service, policy, checker, _, auditor, _ := setupTestService()
	authorizer := NewMemoryPeerAuthorizer(true)
	policy.SetPeerAuthorizer(authorizer)

	agentA, _ := ParseAddress("agent://tenant-1/research/agent-a")
	agentB, _ := ParseAddress("agent://tenant-1/research/agent-b")
	agentC, _ := ParseAddress("agent://tenant-1/research/agent-c")

	checker.Grant(agentA, CapabilitySend)
	checker.Grant(agentB, CapabilityReceive)
	checker.Grant(agentC, CapabilityReceive)

	// Explicitly allow Agent A -> Agent B only
	authorizer.AllowPeer(agentA, PeerRule{
		Namespace: "research",
		Agent:     "agent-b",
	})

	ctx := context.Background()

	// 1. Agent A -> Agent B must SUCCEED (PASS)
	msgAllowed, err := NewMessage(agentA, agentB, MessageTypeRequest, []byte(`{"action":"analyze"}`), time.Minute)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if err := service.Send(ctx, msgAllowed); err != nil {
		t.Fatalf("expected Agent A -> Agent B to be allowed, got error: %v", err)
	}

	// Verify Agent B can receive the message
	received, err := service.Receive(ctx, agentB, 10)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(received) != 1 || received[0].ID != msgAllowed.ID {
		t.Fatalf("expected 1 message in mailbox for Agent B, got %d", len(received))
	}

	// 2. Agent A -> Agent C must be DENIED (Unauthorized Peer Delivery = 0)
	msgDenied, err := NewMessage(agentA, agentC, MessageTypeRequest, []byte(`{"action":"intrude"}`), time.Minute)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if err := service.Send(ctx, msgDenied); !errors.Is(err, ErrPeerDenied) {
		t.Fatalf("expected Agent A -> Agent C to return ErrPeerDenied, got: %v", err)
	}

	// Verify Agent C receives 0 messages
	receivedC, err := service.Receive(ctx, agentC, 10)
	if err != nil {
		t.Fatalf("Receive for agent C: %v", err)
	}
	if len(receivedC) != 0 {
		t.Fatalf("expected 0 messages for unauthorized agent C, got %d", len(receivedC))
	}

	// 3. Verify audit log captures both success and denial trace
	records := auditor.Records()
	foundSuccess := false
	foundDenied := false
	for _, rec := range records {
		if rec.Receiver == agentB.String() && rec.Result == "success" {
			foundSuccess = true
		}
		if rec.Receiver == agentC.String() && rec.Result == "denied" && rec.ReasonCode == "PEER_DENIED" {
			foundDenied = true
		}
	}
	if !foundSuccess || !foundDenied {
		t.Fatalf("audit logs missing expected success (%v) or denial (%v)", foundSuccess, foundDenied)
	}
}

func TestPeerAuthorization_ReceiverPolicy(t *testing.T) {
	t.Parallel()

	service, policy, checker, _, _, _ := setupTestService()
	authorizer := NewMemoryPeerAuthorizer(true)
	policy.SetPeerAuthorizer(authorizer)

	agentA, _ := ParseAddress("agent://tenant-1/ns/agent-a")
	agentB, _ := ParseAddress("agent://tenant-1/ns/agent-b")
	agentC, _ := ParseAddress("agent://tenant-1/ns/agent-c")

	checker.Grant(agentA, CapabilitySend)
	checker.Grant(agentB, CapabilityReceive)
	checker.Grant(agentC, CapabilitySend)

	// Both Agent A and Agent C allow outbound to Agent B
	authorizer.AllowPeer(agentA, PeerRule{Agent: "agent-b"})
	authorizer.AllowPeer(agentC, PeerRule{Agent: "agent-b"})

	// BUT Receiver Agent B defines inbound policy: ONLY Agent A is accepted!
	authorizer.SetReceiverPolicy(agentB, []PeerRule{
		{Agent: "agent-a"},
	})

	ctx := context.Background()

	// 1. Agent A -> Agent B: Allowed by sender allowlist AND receiver policy
	msgA, _ := NewMessage(agentA, agentB, MessageTypeRequest, []byte(`{}`), time.Minute)
	if err := service.Send(ctx, msgA); err != nil {
		t.Fatalf("expected A -> B allowed by receiver policy, got: %v", err)
	}

	// 2. Agent C -> Agent B: Allowed by sender allowlist BUT rejected by receiver policy
	msgC, _ := NewMessage(agentC, agentB, MessageTypeRequest, []byte(`{}`), time.Minute)
	err := service.Send(ctx, msgC)
	if !errors.Is(err, ErrReceiverDenied) {
		t.Fatalf("expected ErrReceiverDenied for C -> B, got: %v", err)
	}
}

func TestPeerAuthorization_CrossTenantDenied(t *testing.T) {
	t.Parallel()

	service, policy, checker, _, _, _ := setupTestService()
	authorizer := NewMemoryPeerAuthorizer(false) // Even if permissive authorizer
	policy.SetPeerAuthorizer(authorizer)

	tenantA, _ := ParseAddress("agent://tenant-alpha/ns/agent-1")
	tenantB, _ := ParseAddress("agent://tenant-beta/ns/agent-2")

	checker.Grant(tenantA, CapabilitySend)
	checker.Grant(tenantB, CapabilityReceive)

	ctx := context.Background()
	msg := &AgentMessage{
		ID:        "msg-cross-tenant-1",
		TenantID:  "tenant-alpha",
		Namespace: "ns",
		Sender:    tenantA,
		Receiver:  tenantB,
		Type:      MessageTypeRequest,
		Payload:   []byte(`{}`),
		CreatedAt: time.Now().UTC(),
		Deadline:  time.Now().UTC().Add(time.Minute),
	}

	err := service.Send(ctx, msg)
	if !errors.Is(err, ErrCrossTenantDenied) {
		t.Fatalf("expected ErrCrossTenantDenied for cross-tenant communication, got: %v", err)
	}
}

func TestPeerAuthorization_FencedStaleInstance(t *testing.T) {
	t.Parallel()

	service, policy, checker, fencer, _, _ := setupTestService()
	policy.AllowPeer(AgentAddress{TenantID: "tenant-1", Namespace: "ns", AgentID: "worker"}, PeerRule{Agent: "receiver"})

	staleSender, _ := ParseAddress("agent://tenant-1/ns/worker/inst-old")
	receiver, _ := ParseAddress("agent://tenant-1/ns/receiver")

	checker.Grant(staleSender, CapabilitySend)
	checker.Grant(receiver, CapabilityReceive)

	// Fence the old instance
	fencer.FenceInstance(staleSender)

	ctx := context.Background()
	msg, err := NewMessage(staleSender, receiver, MessageTypeRequest, []byte(`{}`), time.Minute)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	err = service.Send(ctx, msg)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("expected ErrFenced for stale instance send, got: %v", err)
	}
}

func TestPeerAuthorization_UnauthorizedSignal(t *testing.T) {
	t.Parallel()

	service, policy, checker, _, _, _ := setupTestService()
	sender, _ := ParseAddress("agent://tenant-1/ns/supervisor")
	receiver, _ := ParseAddress("agent://tenant-1/ns/worker")

	policy.AllowPeer(sender, PeerRule{Agent: "worker"})

	// Grant ipc.send and ipc.receive, but NOT ipc.signal
	checker.Grant(sender, CapabilitySend)
	checker.Grant(receiver, CapabilityReceive)

	ctx := context.Background()

	// Dispatching a signal without ipc.signal capability must fail with ErrUnauthorizedSignal
	_, err := service.SendSignal(ctx, sender, receiver, SignalPause, "maintenance", nil)
	if !errors.Is(err, ErrUnauthorizedSignal) {
		t.Fatalf("expected ErrUnauthorizedSignal, got: %v", err)
	}

	// Grant ipc.signal and retry
	checker.Grant(sender, CapabilitySignal)
	sigMsg, err := service.SendSignal(ctx, sender, receiver, SignalPause, "maintenance", nil)
	if err != nil {
		t.Fatalf("expected signal to succeed after grant, got: %v", err)
	}
	if sigMsg.Type != MessageTypeSignal {
		t.Fatalf("expected signal message type, got %s", sigMsg.Type)
	}
}

func TestPeerAuthorization_WildcardAndAuditTrace(t *testing.T) {
	t.Parallel()

	service, policy, checker, _, auditor, _ := setupTestService()
	authorizer := NewMemoryPeerAuthorizer(true)
	policy.SetPeerAuthorizer(authorizer)

	adminAgent, _ := ParseAddress("agent://tenant-1/mgmt/admin-agent")
	worker1, _ := ParseAddress("agent://tenant-1/prod/worker-1")
	worker2, _ := ParseAddress("agent://tenant-1/prod/worker-2")

	checker.Grant(adminAgent, CapabilitySend)
	checker.Grant(worker1, CapabilityReceive)
	checker.Grant(worker2, CapabilityReceive)

	// Allow cross-namespace mgmt -> prod
	policy.AllowCrossNamespace("tenant-1", "mgmt", "prod")

	// Allow admin to talk to any worker in prod namespace via wildcard
	authorizer.AllowPeer(adminAgent, PeerRule{
		Namespace: "prod",
		Agent:     "*",
	})

	ctx := context.Background()

	// Both worker-1 and worker-2 should be reachable
	msg1, _ := NewMessage(adminAgent, worker1, MessageTypeRequest, []byte(`{}`), time.Minute)
	msg1.TraceID = "trace-req-001"
	if err := service.Send(ctx, msg1); err != nil {
		t.Fatalf("admin -> worker1 failed: %v", err)
	}

	msg2, _ := NewMessage(adminAgent, worker2, MessageTypeRequest, []byte(`{}`), time.Minute)
	msg2.TraceID = "trace-req-002"
	if err := service.Send(ctx, msg2); err != nil {
		t.Fatalf("admin -> worker2 failed: %v", err)
	}

	// Verify audit records capture trace IDs and zero payload leakage
	records := auditor.Records()
	foundTrace1 := false
	foundTrace2 := false
	for _, rec := range records {
		if rec.TraceID == "trace-req-001" && rec.Result == "success" {
			foundTrace1 = true
		}
		if rec.TraceID == "trace-req-002" && rec.Result == "success" {
			foundTrace2 = true
		}
		// Zero payload leakage verification: check that reason does not contain payload
		if strings.Contains(rec.Reason, "payload") {
			t.Fatalf("detected potential payload leakage in audit record: %+v", rec)
		}
	}
	if !foundTrace1 || !foundTrace2 {
		t.Fatalf("audit records missing expected traces: trace1=%v, trace2=%v", foundTrace1, foundTrace2)
	}
}
