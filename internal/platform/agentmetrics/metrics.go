// Package agentmetrics owns AgentOS operational metrics. Cardinality is kept
// bounded deliberately: tenant/task/workflow identifiers belong on traces and
// logs, never metric attributes.
package agentmetrics

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var instruments struct {
	sync.Once
	schedulerClaims   metric.Int64Counter
	schedulerOutcomes metric.Int64Counter
	workflowClaims    metric.Int64Counter
	workflowOutcomes  metric.Int64Counter
	spawnOutcomes     metric.Int64Counter
	budgetEvents      metric.Int64Counter
	accountingDrift   metric.Int64Counter
	queueDepth        metric.Int64Histogram
	firstTokenLatency metric.Float64Histogram
}

func initInstruments() {
	instruments.Do(func() {
		meter := otel.Meter("agentos.dev/kernel")
		instruments.schedulerClaims, _ = meter.Int64Counter("agentos.scheduler.claims")
		instruments.schedulerOutcomes, _ = meter.Int64Counter("agentos.scheduler.outcomes")
		instruments.workflowClaims, _ = meter.Int64Counter("agentos.orchestrator.claims")
		instruments.workflowOutcomes, _ = meter.Int64Counter("agentos.orchestrator.outcomes")
		instruments.spawnOutcomes, _ = meter.Int64Counter("agentos.workflow.spawn.outcomes")
		instruments.budgetEvents, _ = meter.Int64Counter("agentos.budget.events")
		instruments.accountingDrift, _ = meter.Int64Counter("agentos.accounting.reconciliation.drift")
		instruments.queueDepth, _ = meter.Int64Histogram("agentos.queue.depth")
		instruments.firstTokenLatency, _ = meter.Float64Histogram("agentos.model.first_token_latency_ms",
			metric.WithUnit("ms"),
			metric.WithDescription("model streaming time-to-first-token, measured at the kernel invoker"))
	})
}

func SchedulerClaims(ctx context.Context, count int) {
	initInstruments()
	instruments.schedulerClaims.Add(ctx, int64(count))
}

func SchedulerOutcome(ctx context.Context, outcome string) {
	initInstruments()
	instruments.schedulerOutcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func WorkflowClaims(ctx context.Context, count int) {
	initInstruments()
	instruments.workflowClaims.Add(ctx, int64(count))
}

func WorkflowOutcome(ctx context.Context, outcome string) {
	initInstruments()
	instruments.workflowOutcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func SpawnOutcome(ctx context.Context, outcome string) {
	initInstruments()
	instruments.spawnOutcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func BudgetEvent(ctx context.Context, event, dimension string) {
	initInstruments()
	instruments.budgetEvents.Add(ctx, 1, metric.WithAttributes(
		attribute.String("event", event), attribute.String("dimension", dimension)))
}

func AccountingDrift(ctx context.Context, ledger string, count int64) {
	if count <= 0 {
		return
	}
	initInstruments()
	instruments.accountingDrift.Add(ctx, count, metric.WithAttributes(attribute.String("ledger", ledger)))
}

func QueueDepth(ctx context.Context, queue string, depth int64) {
	initInstruments()
	instruments.queueDepth.Record(ctx, depth, metric.WithAttributes(attribute.String("queue", queue)))
}

// ModelFirstTokenLatency records streaming time-to-first-token in milliseconds,
// attributed by provider only (bounded cardinality). Recorded at the kernel
// invoker so it is captured regardless of any downstream delta consumer.
func ModelFirstTokenLatency(ctx context.Context, provider string, ms float64) {
	initInstruments()
	instruments.firstTokenLatency.Record(ctx, ms, metric.WithAttributes(attribute.String("provider", provider)))
}
