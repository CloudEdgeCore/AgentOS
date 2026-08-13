package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/admission"
	"github.com/bian-cloud-skill/agentos/internal/kernel/recovery"
	"github.com/bian-cloud-skill/agentos/internal/kernel/scheduler"
	postgresstore "github.com/bian-cloud-skill/agentos/internal/kernel/store/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL (or DATABASE_URL)")
	controllerID := flag.String("controller-id", "", "stable unique controller instance ID")
	poolsFile := flag.String("runtime-pools", "", "JSON runtime pool configuration")
	interval := flag.Duration("interval", 250*time.Millisecond, "reconciliation interval")
	devMode := flag.Bool("dev-mode", false, "acknowledge static development pools and built-in limits")
	flag.Parse()
	if *databaseURL == "" || strings.TrimSpace(*controllerID) == "" || *poolsFile == "" || *interval <= 0 || !*devMode {
		slog.Error("database URL, controller ID, runtime pool file, positive interval, and explicit -dev-mode are required")
		os.Exit(2)
	}
	pools, err := loadPools(*poolsFile)
	if err != nil {
		slog.Error("load runtime pools", "error", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	repository := postgresstore.New(pool)
	engine := admission.New(admission.Limits{
		RuntimeClasses: []string{"wasm", "oci"}, MaxTokens: 1_000_000, MaxCostUSD: 1_000,
		MaxToolCalls: 100_000, MaxWallSeconds: 86_400, MaxCPU: 64_000,
		MaxMemory: 262_144, MaxLLMConcurrency: 128,
	})
	admissionController := admission.NewController(repository, engine, *controllerID+"/admission", 50, 30*time.Second)
	schedulerController := scheduler.NewController(repository, scheduler.StaticPoolSource(pools), *controllerID+"/scheduler", 50, 30*time.Second, 30*time.Second)
	recoveryController := recovery.NewController(repository, 50, 30*time.Second)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		admitted, admissionErr := admissionController.Reconcile(ctx)
		if admissionErr != nil && ctx.Err() == nil {
			slog.Error("admission reconciliation", "error", admissionErr)
		}
		scheduled, schedulerErr := schedulerController.Reconcile(ctx)
		if schedulerErr != nil && ctx.Err() == nil {
			slog.Error("scheduler reconciliation", "error", schedulerErr)
		}
		recovered, recoveryErr := recoveryController.Reconcile(ctx)
		if recoveryErr != nil && ctx.Err() == nil {
			slog.Error("runtime recovery reconciliation", "error", recoveryErr)
		}
		if admitted > 0 || scheduled > 0 || recovered > 0 {
			slog.Info("reconciled tasks", "admitted", admitted, "scheduled", scheduled, "recovered", recovered)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func loadPools(path string) ([]scheduler.RuntimePool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var pools []scheduler.RuntimePool
	if err := decoder.Decode(&pools); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("runtime pool file must contain exactly one JSON document")
	}
	if len(pools) == 0 {
		return nil, fmt.Errorf("at least one runtime pool is required")
	}
	seen := map[string]struct{}{}
	for _, pool := range pools {
		if strings.TrimSpace(pool.ID) == "" || strings.TrimSpace(pool.RuntimeClass) == "" ||
			strings.TrimSpace(pool.RuntimeInstanceID) == "" || strings.TrimSpace(pool.Region) == "" || len(pool.TenantIDs) == 0 {
			return nil, fmt.Errorf("pool ID, runtime class, runtime instance, region, and tenant allowlist are required")
		}
		if _, exists := seen[pool.ID]; exists {
			return nil, fmt.Errorf("duplicate runtime pool ID %q", pool.ID)
		}
		seen[pool.ID] = struct{}{}
	}
	return pools, nil
}
