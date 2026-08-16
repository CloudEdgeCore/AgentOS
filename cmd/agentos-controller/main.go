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
	"github.com/bian-cloud-skill/agentos/internal/kernel/policy"
	"github.com/bian-cloud-skill/agentos/internal/kernel/recovery"
	"github.com/bian-cloud-skill/agentos/internal/kernel/scheduler"
	postgresstore "github.com/bian-cloud-skill/agentos/internal/kernel/store/postgres"
	"github.com/bian-cloud-skill/agentos/internal/platform/otel"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL connection URL (or DATABASE_URL)")
	controllerID := flag.String("controller-id", "", "stable unique controller instance ID")
	poolsFile := flag.String("runtime-pools", "", "JSON runtime pool configuration")
	tenantPoliciesFile := flag.String("tenant-policies", "", "JSON tenant policy data (absent tenants are denied by default)")
	interval := flag.Duration("interval", 250*time.Millisecond, "reconciliation interval")
	// poolHealthFreshness is the lease heartbeat freshness window for pool
	// health (v0.6): a pool whose worker holds an unreleased lease that was
	// not renewed within this window (or expired) is not ready for
	// placement. Keep it comfortably above the scheduler lease TTL.
	poolHealthFreshness := flag.Duration("pool-health-freshness", 90*time.Second, "lease heartbeat freshness window for pool health")
	// Tenant-consistent sharding (ADR-016): with -shard-count N > 0, this
	// instance claims only tenants whose md5 hash maps to -shard-index.
	// Every instance must use the same count; mismatch stalls tasks
	// fail-visibly instead of misprocessing them.
	shardIndex := flag.Int("shard-index", 0, "tenant-consistent claim shard index (ADR-016)")
	shardCount := flag.Int("shard-count", 0, "tenant-consistent claim shard count; 0 disables sharding")
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
	tenantPolicies := policy.TenantPolicies{}
	if *tenantPoliciesFile != "" {
		tenantPolicies, err = loadTenantPolicies(*tenantPoliciesFile)
		if err != nil {
			slog.Error("load tenant policies", "error", err)
			os.Exit(2)
		}
	}
	policyEngine, err := policy.New(tenantPolicies)
	if err != nil {
		slog.Error("prepare policy engine", "error", err)
		os.Exit(1)
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
	repository := postgresstore.New(pool)
	engine := admission.New(admission.Limits{
		RuntimeClasses: []string{"wasm", "oci"}, MaxTokens: 1_000_000, MaxCostUSD: 1_000,
		MaxToolCalls: 100_000, MaxWallSeconds: 86_400, MaxCPU: 64_000,
		MaxMemory: 262_144, MaxLLMConcurrency: 128,
		// Container classes must declare explicit sandbox limits; zero values
		// are never admitted (hardening checklist §4.1).
		ContainerClasses: []string{"oci"},
	})
	admissionController := admission.NewController(repository, engine, policyEngine, *controllerID+"/admission", 50, 30*time.Second)
	// Runtime-aware pool health (v0.6): static pool configuration is
	// overlaid with lease-derived instance liveness, so a pool whose worker
	// stopped renewing its lease is rejected by placement.
	poolSource := scheduler.NewLeaseAwarePoolSource(scheduler.StaticPoolSource(pools), repository, *poolHealthFreshness)
	schedulerController := scheduler.NewController(repository, poolSource, *controllerID+"/scheduler", 50, 30*time.Second, 30*time.Second)
	// Tenant-consistent sharding (ADR-016): admission and scheduling must
	// share the same shard so one instance owns a tenant's whole pipeline.
	if *shardCount > 0 {
		if *shardIndex < 0 || *shardIndex >= *shardCount {
			slog.Error("shard index must satisfy 0 <= index < count", "shardIndex", *shardIndex, "shardCount", *shardCount)
			os.Exit(2)
		}
		admission.WithShard(*shardIndex, *shardCount)(admissionController)
		scheduler.WithShard(*shardIndex, *shardCount)(schedulerController)
		slog.Info("controller instance is sharded", "shardIndex", *shardIndex, "shardCount", *shardCount, "controllerID", *controllerID)
	} else {
		slog.Warn("controller instance is NOT sharded (claims every tenant; ADR-016)", "controllerID", *controllerID)
	}
	recoveryController := recovery.NewController(repository, 50, 30*time.Second)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		admitted, admissionErr := admissionController.Reconcile(ctx)
		scheduled, schedulerErr := schedulerController.Reconcile(ctx)
		recovered, recoveryErr := recoveryController.Reconcile(ctx)
		if admissionErr != nil && ctx.Err() == nil {
			slog.Error("admission reconciliation", "error", admissionErr)
		}
		if schedulerErr != nil && ctx.Err() == nil {
			slog.Error("scheduler reconciliation", "error", schedulerErr)
		}
		if recoveryErr != nil && ctx.Err() == nil {
			slog.Error("runtime recovery reconciliation", "error", recoveryErr)
		}
		if admitted > 0 || scheduled > 0 || recovered > 0 {
			slog.Info("reconciled tasks", "admitted", admitted, "scheduled", scheduled, "recovered", recovered)
			continue
		}
		if (admissionErr != nil || schedulerErr != nil || recoveryErr != nil) && ctx.Err() == nil {
			// A failing reconcile must not hot-loop: back off one interval
			// before the next attempt.
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func loadTenantPolicies(path string) (policy.TenantPolicies, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policies policy.TenantPolicies
	if err := decoder.Decode(&policies); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("tenant policy file must contain exactly one JSON document")
	}
	return policies, nil
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
