// Real model invocation: Invoker binds the fenced decision
// chain (Begin → Settle → Finish) to a provider execution layer. The gateway
// still owns policy, budget, the call ledger and cost computation; the
// executor owns the wire. Content flows through and is never persisted — only
// metadata (usage, cost, provider request id, finish reason) reaches the
// ledger and the audit receipt.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/model/provider"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
)

// ErrNoProviderExecution reports a model whose provider has no execution
// endpoint configured; the invocation fails closed before any ledger row or
// budget consumption is opened.
var ErrNoProviderExecution = errors.New("model provider has no execution endpoint configured")

// InvokeInput is one fenced real model invocation.
type InvokeInput struct {
	TenantID        string
	TaskID          uuid.UUID
	RunID           uuid.UUID
	AttemptID       uuid.UUID
	FencingToken    int64
	AgentVersionRef string
	ModelRef        string
	IdempotencyKey  string

	Messages        []provider.Message
	Temperature     *float64
	MaxOutputTokens int32
	Stream          bool
}

// InvokeOutput carries the terminal ledger row and the completion content.
type InvokeOutput struct {
	Call      store.ModelCall
	Content   string
	ToolCalls []provider.ToolCall
}

// midStreamSettleTokens is the estimated-token granularity of the hard-stop
// guard during streaming: accumulated output is settled in increments so a
// runaway stream trips the budget ceiling before Finish. The final Finish
// settlement corrects the estimate to the provider's exact usage.
const midStreamSettleTokens = 128

// Invoker executes real model calls behind the Model Gateway decision chain.
type Invoker struct {
	gateway   *Gateway
	providers *provider.Registry
	now       func() time.Time
}

// NewInvoker binds the decision gateway to a provider registry. A nil
// registry fails every invocation closed (governance without execution).
func NewInvoker(gateway *Gateway, providers *provider.Registry) *Invoker {
	if providers == nil {
		providers = provider.NewRegistry()
	}
	return &Invoker{gateway: gateway, providers: providers, now: time.Now}
}

// InvokeStream performs one invocation with streaming intent: content deltas
// are delivered to onDelta as they arrive (nil disables delivery). A
// descriptor that declares no streaming support transparently falls back to
// the non-streaming wire call. When the budget ceiling trips mid-stream the
// provider call is cancelled, the ledger row finishes STOPPED, and
// ErrBudgetExhausted is returned.
func (inv *Invoker) InvokeStream(ctx context.Context, in InvokeInput, onDelta func(string)) (InvokeOutput, error) {
	in.Stream = true
	return inv.invoke(ctx, in, onDelta)
}

// Invoke performs one non-streaming invocation.
func (inv *Invoker) Invoke(ctx context.Context, in InvokeInput) (InvokeOutput, error) {
	in.Stream = false
	return inv.invoke(ctx, in, nil)
}

func (inv *Invoker) invoke(ctx context.Context, in InvokeInput, onDelta func(string)) (InvokeOutput, error) {
	if in.MaxOutputTokens < 0 {
		return InvokeOutput{}, fmt.Errorf("max output tokens must not be negative")
	}
	providerName, _, _ := strings.Cut(in.ModelRef, "/")
	executor, wireModel, err := inv.providers.ResolveModel(in.ModelRef)
	if err != nil {
		return InvokeOutput{}, fmt.Errorf("%w: %s", ErrNoProviderExecution, providerName)
	}

	begin, err := inv.gateway.Begin(ctx, BeginInput{
		TenantID: in.TenantID, TaskID: in.TaskID, RunID: in.RunID, AttemptID: in.AttemptID,
		FencingToken: in.FencingToken, AgentVersionRef: in.AgentVersionRef,
		ModelRef: in.ModelRef, IdempotencyKey: in.IdempotencyKey,
		EstimatedInputTokens: estimateInputTokens(in.Messages), MaxOutputTokens: int64(in.MaxOutputTokens),
	})
	if err != nil {
		return InvokeOutput{}, err
	}
	in.MaxOutputTokens = int32(begin.EffectiveMaxOutputTokens)
	call := begin.Call

	// The hard-stop guard: stream deltas are settled as estimated increments
	// so the ceiling trips without waiting for the final usage report.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	sequence := int64(0)
	pendingBytes := int64(0)
	settledEstimate := int64(0)
	var exhausted error
	guardedGenerated := func(delta string) {
		if exhausted != nil {
			return
		}
		pendingBytes += int64(len(delta))
		estimatedTokens := pendingBytes / 4
		increment := estimatedTokens - settledEstimate
		if increment < midStreamSettleTokens {
			return
		}
		sequence++
		settledEstimate = estimatedTokens
		if err := inv.gateway.Settle(streamCtx, call, sequence, Usage{OutputTokens: increment}); err != nil {
			if errors.Is(err, ErrBudgetExhausted) {
				exhausted = err
				cancelStream()
				return
			}
			// Settlement infrastructure failures must not silently unbound
			// the stream; treat them as fatal too.
			exhausted = err
			cancelStream()
		}
	}

	invocation := provider.Invocation{
		ModelName: wireModel, Messages: in.Messages,
		Temperature: in.Temperature, MaxOutputTokens: in.MaxOutputTokens, Stream: in.Stream,
		IdempotencyKey: in.IdempotencyKey, TaskID: in.TaskID.String(), AttemptID: in.AttemptID.String(),
		ModelCallID: call.ID.String(),
	}
	var result provider.Result
	if in.Stream && !begin.Descriptor.SupportsStreaming {
		// The tenant descriptor declares the capability; honor it.
		result, err = executor.Complete(streamCtx, invocation)
	} else if in.Stream {
		result, err = executor.StreamObserved(streamCtx, invocation, provider.StreamObserver{
			OnContent: onDelta, OnGenerated: guardedGenerated,
		})
	} else {
		result, err = executor.Complete(streamCtx, invocation)
	}
	if exhausted != nil {
		return inv.finishGuarded(ctx, call, result, exhausted)
	}
	if err != nil {
		// Provider failure: definitive rejection/overload is known zero usage;
		// transport loss, 5xx, and partial streams are explicitly unknown and
		// enter reconciliation instead of being silently treated as free.
		finishReason := "provider_error"
		if finish := providerFinishReason(err); finish != "" {
			finishReason = finish
		}
		usageCertainty := store.ModelUsageUnknown
		if provider.UsageKnownZero(err) {
			usageCertainty = store.ModelUsageKnownZero
		}
		failed, finishErr := inv.gateway.Finish(ctx, call, FinishInput{
			TenantID: call.TenantID, ModelCallID: call.ID, ExpectedVersion: call.ResourceVersion,
			Status: store.ModelCallFailed, ProviderRequestID: result.ProviderRequestID,
			FinishReason: finishReason, UsageCertainty: usageCertainty,
		})
		if finishErr != nil && !errors.Is(finishErr, ErrBudgetExhausted) {
			return InvokeOutput{}, errors.Join(err, finishErr)
		}
		return InvokeOutput{Call: failed}, err
	}

	usageCertainty := store.ModelUsageUnknown
	if result.UsageReported {
		usageCertainty = store.ModelUsageKnownZero
		if result.InputTokens+result.OutputTokens > 0 {
			usageCertainty = store.ModelUsageKnown
		}
	}
	finished, err := inv.gateway.Finish(ctx, call, FinishInput{
		TenantID: call.TenantID, ModelCallID: call.ID, ExpectedVersion: call.ResourceVersion,
		Status: store.ModelCallCompleted, InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		ProviderRequestID: result.ProviderRequestID, FinishReason: result.FinishReason,
		UsageCertainty: usageCertainty,
	})
	output := InvokeOutput{Call: finished, Content: result.Content, ToolCalls: result.ToolCalls}
	if err != nil {
		return output, err
	}
	return output, nil
}

func estimateInputTokens(messages []provider.Message) int64 {
	var bytes int64
	for _, message := range messages {
		bytes += int64(len(message.Role) + len(message.Content) + len(message.ToolCallID))
		for _, call := range message.ToolCalls {
			bytes += int64(len(call.ID) + len(call.Name) + len(call.Arguments))
		}
	}
	if bytes == 0 {
		return 0
	}
	return (bytes + 3) / 4
}

// finishGuarded closes a stream that the budget guard cancelled: exact usage
// is unknown, so the row finishes STOPPED with the provider-reported usage if
// any arrived before cancellation.
func (inv *Invoker) finishGuarded(ctx context.Context, call store.ModelCall, result provider.Result, cause error) (InvokeOutput, error) {
	usageCertainty := store.ModelUsageUnknown
	if result.UsageReported {
		usageCertainty = store.ModelUsageKnownZero
		if result.InputTokens+result.OutputTokens > 0 {
			usageCertainty = store.ModelUsageKnown
		}
	}
	finished, err := inv.gateway.Finish(ctx, call, FinishInput{
		TenantID: call.TenantID, ModelCallID: call.ID, ExpectedVersion: call.ResourceVersion,
		Status: store.ModelCallStopped, InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
		ProviderRequestID: result.ProviderRequestID, FinishReason: "budget_guard",
		UsageCertainty: usageCertainty,
	})
	if err != nil && !errors.Is(err, ErrBudgetExhausted) {
		return InvokeOutput{}, errors.Join(cause, err)
	}
	return InvokeOutput{Call: finished}, cause
}

// providerFinishReason maps executor error classes onto bounded ledger
// finish-reason strings.
func providerFinishReason(err error) string {
	switch {
	case errors.Is(err, provider.ErrProviderUnavailable):
		return "provider_unavailable"
	case errors.Is(err, provider.ErrProviderRejected):
		return "provider_rejected"
	case errors.Is(err, provider.ErrStreamAborted):
		return "provider_stream_aborted"
	default:
		return ""
	}
}
