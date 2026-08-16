// Command agentos-projector consumes canonical store events from JetStream
// and applies them to the OpenSearch search projection (ADR-013). Memory
// upserts index documents; memory tombstones delete them, propagating
// deletions from the canonical store to the search index. Application is
// idempotent by resource version, so replays and redeliveries are safe.
//
// Usage:
//
//	agentos-projector -database-url ... -nats-url nats://127.0.0.1:54222 \
//	    -opensearch-addr http://127.0.0.1:39200 -opensearch-index agentos-memory
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	postgresstore "github.com/bian-cloud-skill/agentos/internal/kernel/store/postgres"
	"github.com/bian-cloud-skill/agentos/internal/platform/opensearch"
	"github.com/bian-cloud-skill/agentos/internal/platform/otel"
	"github.com/bian-cloud-skill/agentos/internal/platform/outbox"
	"github.com/bian-cloud-skill/agentos/internal/platform/projector"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const memoryFilter = outbox.SubjectPrefix + ".memory.>"

func main() {
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL (or DATABASE_URL)")
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL")
	opensearchAddr := flag.String("opensearch-addr", "http://127.0.0.1:39200", "OpenSearch cluster address")
	opensearchUser := flag.String("opensearch-user", "", "OpenSearch basic auth user (security plugin)")
	opensearchPassword := flag.String("opensearch-password", "", "OpenSearch basic auth password (security plugin)")
	opensearchIndex := flag.String("opensearch-index", "agentos-memory", "OpenSearch index for projected memory records")
	consumerID := flag.String("consumer-id", "agentos-projector-memory", "durable JetStream consumer ID")
	replicas := flag.Int("stream-replicas", 1, "JetStream replica count")
	flag.Parse()
	if *databaseURL == "" {
		slog.Error("database URL is required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTelemetry, err := otel.Init(ctx)
	if err != nil {
		slog.Error("initialize OpenTelemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shutdownTelemetry(shutdownCtx)
	}()
	pool, err := pgxpool.New(ctx, *databaseURL)
	if err != nil {
		slog.Error("create PostgreSQL pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		slog.Error("connect to PostgreSQL", "error", err)
		os.Exit(1)
	}
	natsConnection, err := nats.Connect(*natsURL, nats.Name("agentos-projector-"+*consumerID), nats.Timeout(5*time.Second))
	if err != nil {
		slog.Error("connect to NATS", "error", err)
		os.Exit(1)
	}
	defer natsConnection.Close()
	js, err := jetstream.New(natsConnection)
	if err != nil {
		slog.Error("create JetStream context", "error", err)
		os.Exit(1)
	}
	if err := outbox.EnsureStream(ctx, js, *replicas); err != nil {
		slog.Error("configure event stream", "error", err)
		os.Exit(1)
	}

	var options []opensearch.Option
	if *opensearchUser != "" {
		options = append(options, opensearch.WithCredentials(*opensearchUser, *opensearchPassword))
	}
	searchClient, err := opensearch.New(*opensearchAddr, *opensearchIndex, options...)
	if err != nil {
		slog.Error("configure OpenSearch client", "error", err)
		os.Exit(1)
	}
	pingCtx, cancelPing := context.WithTimeout(ctx, 15*time.Second)
	if err := searchClient.Ping(pingCtx); err != nil {
		cancelPing()
		slog.Error("OpenSearch is not reachable", "error", err)
		os.Exit(1)
	}
	cancelPing()
	indexCtx, cancelIndex := context.WithTimeout(ctx, 15*time.Second)
	defer cancelIndex()
	if err := searchClient.EnsureIndex(indexCtx, projector.MemoryMapping); err != nil {
		slog.Error("ensure OpenSearch index", "error", err)
		os.Exit(1)
	}
	memoryProjector := projector.NewMemoryProjector(postgresstore.New(pool), searchClient)

	consumer, err := js.CreateOrUpdateConsumer(ctx, outbox.StreamName, jetstream.ConsumerConfig{
		Durable:       *consumerID,
		Name:          *consumerID,
		FilterSubject: memoryFilter,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxAckPending: 256,
	})
	if err != nil {
		slog.Error("create JetStream consumer", "error", err)
		os.Exit(1)
	}
	consumeContext, err := consumer.Consume(func(message jetstream.Msg) {
		if err := applyMessage(ctx, memoryProjector, message); err != nil {
			slog.Error("projection apply failed; retrying", "error", err, "subject", message.Subject())
			_ = message.NakWithDelay(2 * time.Second)
			return
		}
		_ = message.Ack()
	})
	if err != nil {
		slog.Error("start JetStream consumer", "error", err)
		os.Exit(1)
	}
	defer consumeContext.Stop()
	slog.Info("Agent OS memory projector listening", "index", *opensearchIndex, "filter", memoryFilter, "consumer", *consumerID)
	<-ctx.Done()
}

func applyMessage(ctx context.Context, memoryProjector *projector.MemoryProjector, message jetstream.Msg) error {
	envelope, err := decodeEnvelope(message.Data())
	if err != nil {
		// A malformed envelope can never succeed; drop it instead of
		// redelivering forever.
		slog.Error("dropping malformed projection envelope", "error", err, "subject", message.Subject())
		_ = message.Ack()
		return nil
	}
	event := store.OutboxEvent{
		ID: envelope.EventID, TenantID: envelope.TenantID,
		AggregateType: envelope.AggregateType, AggregateID: envelope.AggregateID,
		AggregateVersion: envelope.AggregateVersion, EventType: envelope.EventType,
		Payload: envelope.Payload, OccurredAt: envelope.OccurredAt,
	}
	applyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return memoryProjector.Apply(applyCtx, event)
}

type eventEnvelope struct {
	EventID          uuid.UUID       `json:"eventId"`
	TenantID         string          `json:"tenantId"`
	AggregateType    string          `json:"aggregateType"`
	AggregateID      uuid.UUID       `json:"aggregateId"`
	AggregateVersion int64           `json:"aggregateVersion"`
	EventType        string          `json:"eventType"`
	OccurredAt       time.Time       `json:"occurredAt"`
	Payload          json.RawMessage `json:"payload"`
}

func decodeEnvelope(data []byte) (eventEnvelope, error) {
	var envelope eventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return envelope, err
	}
	if envelope.EventID == uuid.Nil || envelope.TenantID == "" || envelope.AggregateID == uuid.Nil || len(envelope.Payload) == 0 {
		return envelope, errors.New("projection envelope is incomplete")
	}
	return envelope, nil
}
