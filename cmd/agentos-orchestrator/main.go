// Command agentos-orchestrator runs the v1.2 workflow orchestrator: it
// decides which workflow steps execute when and creates ordinary Tasks for
// them. Scheduling (where a Task runs) stays with the scheduler. The
// reconcile loop is stateless and durable — restarts and concurrent
// instances converge without double dispatch.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/store/postgres"
	"github.com/CloudEdgeCore/AgentOS/internal/kernel/workflow"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/artifact"
	"github.com/CloudEdgeCore/AgentOS/internal/platform/otel"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL (or DATABASE_URL)")
	orchestratorID := flag.String("orchestrator-id", "", "unique orchestrator instance identity")
	artifactRoot := flag.String("artifact-root", "", "content-addressed artifact directory (task result reading)")
	interval := flag.Duration("interval", 250*time.Millisecond, "reconcile interval")
	batch := flag.Int("batch", 100, "workflows per reconcile batch (1..100)")
	parallel := flag.Int("parallel", 4, "concurrent workflows per batch")
	flag.Parse()
	if strings.TrimSpace(*databaseURL) == "" || strings.TrimSpace(*orchestratorID) == "" ||
		strings.TrimSpace(*artifactRoot) == "" || *interval <= 0 || *batch <= 0 || *batch > 100 {
		slog.Error("database URL, orchestrator id, artifact root, and positive bounds are required (batch 1..100)")
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

	config, err := pgxpool.ParseConfig(*databaseURL)
	if err != nil {
		slog.Error("parse database URL", "error", err)
		os.Exit(2)
	}
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		slog.Error("open PostgreSQL", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		slog.Error("ping PostgreSQL", "error", err)
		os.Exit(1)
	}

	artifacts, err := artifact.NewFilesystem(*artifactRoot, 64<<20)
	if err != nil {
		slog.Error("create artifact store", "error", err)
		os.Exit(1)
	}
	repository := postgres.New(pool)
	controller := workflow.NewController(repository, repository, artifacts, *orchestratorID, *batch).
		WithParallelism(*parallel)

	slog.Info("workflow orchestrator running",
		"orchestratorId", *orchestratorID, "interval", interval.String(), "batch", *batch)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		processed, err := controller.Reconcile(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("workflow reconcile", "error", err)
		}
		if processed > 0 {
			slog.Debug("workflow reconcile advanced", "workflows", processed)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
