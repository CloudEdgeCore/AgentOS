package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestIPCAddressCanonicalRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []AgentAddress{
		{AgentVersionRef: "planner@1.2.0", Instance: "3f5c0b7e-6d31-4a19-9c50-3f0c6b1d9a44"},
		{AgentVersionRef: "research/summarizer@2026.09", Instance: "0b0c8f1a-9d2e-4f3b-8a11-7c6e5d4b3a21"},
	}
	for _, address := range cases {
		canonical := address.Canonical()
		parsed, err := ParseAgentAddress(canonical)
		if err != nil {
			t.Fatalf("parse %q: %v", canonical, err)
		}
		if parsed != address {
			t.Fatalf("round trip of %q produced %+v, want %+v", canonical, parsed, address)
		}
		if parsed.Canonical() != canonical {
			t.Fatalf("re-rendering %q produced %q", canonical, parsed.Canonical())
		}
	}
}

func TestIPCAddressRejectsUnscopedAndAmbiguousForms(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  error
	}{
		{"version only", "planner@1.2.0", ErrIPCAddressUnscoped},
		{"trailing separator", "planner@1.2.0#", ErrIPCAddressUnscoped},
		{"empty", "", ErrIPCAddressUnscoped},
		{"separator inside instance", "planner@1.2.0#run#1", nil},
		{"missing version reference", "#run-1", nil},
	}
	for _, tc := range cases {
		address, err := ParseAgentAddress(tc.input)
		if err == nil {
			t.Errorf("%s: parsed %q into %+v, want an error", tc.name, tc.input, address)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: error is %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestAgentAddressValidateRejectsSeparatorInsideASegment(t *testing.T) {
	t.Parallel()

	if err := (AgentAddress{AgentVersionRef: "a#b@1", Instance: "run"}).Validate(); err == nil {
		t.Fatal("an agent version reference containing the separator was accepted")
	}
	if err := (AgentAddress{AgentVersionRef: "a@1", Instance: "ru#n"}).Validate(); err == nil {
		t.Fatal("an instance containing the separator was accepted")
	}
	if err := (AgentAddress{AgentVersionRef: "a@1", Instance: "run"}).Validate(); err != nil {
		t.Fatalf("a well formed address was rejected: %v", err)
	}
	if err := (AgentAddress{AgentVersionRef: "a@1", Instance: strings.Repeat("x", ipcMaxTextFieldLength+1)}).Validate(); err == nil {
		t.Fatal("an over long instance was accepted")
	}
}

func TestIPCMailboxConsumerNameIsScoped(t *testing.T) {
	t.Parallel()

	if got, want := IPCMailboxConsumerName("run-1"), "run:run-1"; got != want {
		t.Fatalf("consumer name is %q, want %q", got, want)
	}
}

// validAppendInput is the baseline every rejection case is derived from.
func validAppendInput() AppendIPCMessageInput {
	return AppendIPCMessageInput{
		TenantID:            "tenant-a",
		ID:                  uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		FromAgentVersionRef: "planner@1.2.0",
		To:                  AgentAddress{AgentVersionRef: "worker@1.0.0", Instance: "22222222-2222-4222-8222-222222222222"},
		Kind:                "handoff",
		Payload:             json.RawMessage(`{"goal":"summarize"}`),
		IdempotencyKey:      "send-1",
		SentAt:              time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
	}
}

func TestAppendIPCMessageInputValidate(t *testing.T) {
	t.Parallel()

	if err := validAppendInput().Validate(); err != nil {
		t.Fatalf("the baseline message was rejected: %v", err)
	}

	withDeadline := validAppendInput()
	withDeadline.Deadline = withDeadline.SentAt.Add(time.Minute)
	if err := withDeadline.Validate(); err != nil {
		t.Fatalf("a message with a future deadline was rejected: %v", err)
	}

	withAttachments := validAppendInput()
	withAttachments.Attachments = json.RawMessage(`[{"uri":"s3://bucket/key","sha256":"abc","sizeBytes":12,"mediaType":"text/plain"}]`)
	if err := withAttachments.Validate(); err != nil {
		t.Fatalf("a message with attachments was rejected: %v", err)
	}

	oversized := validAppendInput()
	oversized.Payload = json.RawMessage(`"` + strings.Repeat("x", IPCPayloadLimit) + `"`)

	cases := []struct {
		name  string
		apply func(*AppendIPCMessageInput)
		want  error
	}{
		{"missing tenant", func(in *AppendIPCMessageInput) { in.TenantID = "  " }, nil},
		{"missing message id", func(in *AppendIPCMessageInput) { in.ID = uuid.Nil }, nil},
		{"missing sender", func(in *AppendIPCMessageInput) { in.FromAgentVersionRef = "" }, nil},
		{"unscoped mailbox", func(in *AppendIPCMessageInput) { in.To.Instance = "" }, ErrIPCAddressUnscoped},
		{"missing kind", func(in *AppendIPCMessageInput) { in.Kind = "" }, nil},
		{"payload not JSON", func(in *AppendIPCMessageInput) { in.Payload = json.RawMessage(`{`) }, nil},
		{"empty payload", func(in *AppendIPCMessageInput) { in.Payload = nil }, nil},
		{"payload above the limit", func(in *AppendIPCMessageInput) { in.Payload = oversized.Payload }, ErrIPCPayloadTooLarge},
		{"attachments not an array", func(in *AppendIPCMessageInput) { in.Attachments = json.RawMessage(`{"uri":"x"}`) }, nil},
		{"attachments malformed", func(in *AppendIPCMessageInput) { in.Attachments = json.RawMessage(`[`) }, nil},
		{"missing idempotency key", func(in *AppendIPCMessageInput) { in.IdempotencyKey = "" }, nil},
		{"missing sent at", func(in *AppendIPCMessageInput) { in.SentAt = time.Time{} }, nil},
		{"deadline equals sent at", func(in *AppendIPCMessageInput) { in.Deadline = in.SentAt }, ErrIPCDeadlineElapsed},
		{"deadline before sent at", func(in *AppendIPCMessageInput) { in.Deadline = in.SentAt.Add(-time.Second) }, ErrIPCDeadlineElapsed},
	}
	for _, tc := range cases {
		input := validAppendInput()
		tc.apply(&input)
		err := input.Validate()
		if err == nil {
			t.Errorf("%s: rejected input was accepted", tc.name)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: error is %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestAppendIPCMessageDeadlineVerdictDoesNotDependOnTheClock pins the property a
// retry depends on: whether a send is rejected is a function of the two supplied
// timestamps, so replaying the same logical send hours later reaches the same
// verdict and reaches the idempotency path rather than failing validation.
func TestAppendIPCMessageDeadlineVerdictDoesNotDependOnTheClock(t *testing.T) {
	t.Parallel()

	input := validAppendInput()
	input.Deadline = input.SentAt.Add(time.Minute)
	first := input.Validate()
	second := input.Validate()
	if first != nil || second != nil {
		t.Fatalf("a valid message was rejected on repeat: %v / %v", first, second)
	}

	expired := validAppendInput()
	expired.Deadline = expired.SentAt.Add(-time.Minute)
	if err := expired.Validate(); !errors.Is(err, ErrIPCDeadlineElapsed) {
		t.Fatalf("an expired message reported %v, want %v", err, ErrIPCDeadlineElapsed)
	}
	if err := expired.Validate(); !errors.Is(err, ErrIPCDeadlineElapsed) {
		t.Fatalf("the deadline verdict changed between calls: %v", err)
	}
}

func TestListMailboxInputValidate(t *testing.T) {
	t.Parallel()

	valid := ListMailboxInput{
		TenantID:     "tenant-a",
		To:           AgentAddress{AgentVersionRef: "worker@1.0.0", Instance: "22222222-2222-4222-8222-222222222222"},
		Limit:        10,
		ConsumerName: IPCMailboxConsumerName("22222222-2222-4222-8222-222222222222"),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid drain was rejected: %v", err)
	}

	cases := []struct {
		name  string
		apply func(*ListMailboxInput)
	}{
		{"missing tenant", func(in *ListMailboxInput) { in.TenantID = "" }},
		{"unscoped mailbox", func(in *ListMailboxInput) { in.To.Instance = "" }},
		{"negative cursor", func(in *ListMailboxInput) { in.AfterSequence = -1 }},
		{"zero limit", func(in *ListMailboxInput) { in.Limit = 0 }},
		{"limit above the batch bound", func(in *ListMailboxInput) { in.Limit = ipcMaxBatchSize + 1 }},
		{"missing consumer", func(in *ListMailboxInput) { in.ConsumerName = "" }},
	}
	for _, tc := range cases {
		input := valid
		tc.apply(&input)
		if err := input.Validate(); err == nil {
			t.Errorf("%s: rejected drain was accepted", tc.name)
		}
	}
}

func TestAcknowledgeIPCMessagesInputValidate(t *testing.T) {
	t.Parallel()

	first := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	second := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	valid := AcknowledgeIPCMessagesInput{
		TenantID:       "tenant-a",
		ConsumerName:   IPCMailboxConsumerName("run-1"),
		MessageIDs:     []uuid.UUID{first, second},
		AcknowledgedAt: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid acknowledgement was rejected: %v", err)
	}

	cases := []struct {
		name  string
		apply func(*AcknowledgeIPCMessagesInput)
	}{
		{"missing tenant", func(in *AcknowledgeIPCMessagesInput) { in.TenantID = "" }},
		{"missing consumer", func(in *AcknowledgeIPCMessagesInput) { in.ConsumerName = "" }},
		{"no message ids", func(in *AcknowledgeIPCMessagesInput) { in.MessageIDs = nil }},
		{"nil message id", func(in *AcknowledgeIPCMessagesInput) { in.MessageIDs = []uuid.UUID{uuid.Nil} }},
		{"duplicate message id", func(in *AcknowledgeIPCMessagesInput) { in.MessageIDs = []uuid.UUID{first, first} }},
		{"missing acknowledged at", func(in *AcknowledgeIPCMessagesInput) { in.AcknowledgedAt = time.Time{} }},
	}
	for _, tc := range cases {
		input := valid
		tc.apply(&input)
		if err := input.Validate(); err == nil {
			t.Errorf("%s: rejected acknowledgement was accepted", tc.name)
		}
	}
}
