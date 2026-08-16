// Package scheduler provides deterministic, explainable runtime placement.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bian-cloud-skill/agentos/internal/kernel/store"
	"github.com/bian-cloud-skill/agentos/internal/kernel/workload"
	"github.com/google/uuid"
)

var ErrNoPlacement = errors.New("no eligible runtime placement")
var ErrInvalidPoolSet = errors.New("runtime pool set is invalid")

type RuntimePool struct {
	ID                string   `json:"id"`
	TenantIDs         []string `json:"tenantIds"`
	RuntimeClass      string   `json:"runtimeClass"`
	RuntimeInstanceID string   `json:"runtimeInstanceId"`
	Region            string   `json:"region"`
	DataResidency     string   `json:"dataResidency"`
	Ready             bool     `json:"ready"`
	AvailableCPU      int64    `json:"availableCpuMillis"`
	AvailableMemory   int64    `json:"availableMemoryMiB"`
	AvailableLLMSlots int      `json:"availableLlmSlots"`
	ArtifactRegions   []string `json:"artifactRegions"`
	CostWeight        float64  `json:"costWeight"`
}

type ScoreComponent struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Reason string  `json:"reason"`
}

type Placement struct {
	Pool       RuntimePool      `json:"pool"`
	Score      float64          `json:"score"`
	Components []ScoreComponent `json:"components"`
}

type Rejection struct {
	PoolID  string   `json:"poolId"`
	Reasons []string `json:"reasons"`
}

type Result struct {
	Placement Placement   `json:"placement"`
	Rejected  []Rejection `json:"rejected"`
}

func Select(spec workload.Spec, pools []RuntimePool) (Result, error) {
	var candidates []Placement
	result := Result{}
	seen := make(map[string]struct{}, len(pools))
	for _, pool := range pools {
		var rejected []string
		if strings.TrimSpace(pool.ID) == "" || strings.TrimSpace(pool.RuntimeInstanceID) == "" {
			return result, fmt.Errorf("%w: pool ID and runtime instance ID are required", ErrInvalidPoolSet)
		}
		if _, exists := seen[pool.ID]; exists {
			return result, fmt.Errorf("%w: duplicate pool ID %q", ErrInvalidPoolSet, pool.ID)
		}
		seen[pool.ID] = struct{}{}
		if !pool.Ready {
			rejected = append(rejected, "POOL_NOT_READY")
		}
		if !slices.Contains(spec.Placement.RuntimeClasses, pool.RuntimeClass) {
			rejected = append(rejected, "RUNTIME_CLASS_MISMATCH")
		}
		if pool.Region != spec.Placement.Region {
			rejected = append(rejected, "REGION_MISMATCH")
		}
		if spec.Placement.DataResidency != "" && pool.DataResidency != spec.Placement.DataResidency {
			rejected = append(rejected, "DATA_RESIDENCY_MISMATCH")
		}
		if pool.AvailableCPU < spec.Placement.CPU {
			rejected = append(rejected, "CPU_EXHAUSTED")
		}
		if pool.AvailableMemory < spec.Placement.Memory {
			rejected = append(rejected, "MEMORY_EXHAUSTED")
		}
		if pool.AvailableLLMSlots < spec.Placement.LLMConcurrency {
			rejected = append(rejected, "LLM_CAPACITY_EXHAUSTED")
		}
		if len(rejected) != 0 {
			result.Rejected = append(result.Rejected, Rejection{PoolID: pool.ID, Reasons: rejected})
			continue
		}
		components := score(spec, pool)
		var total float64
		for _, component := range components {
			total += component.Value
		}
		candidates = append(candidates, Placement{Pool: pool, Score: total, Components: components})
	}
	if len(candidates) == 0 {
		sort.Slice(result.Rejected, func(i, j int) bool { return result.Rejected[i].PoolID < result.Rejected[j].PoolID })
		return result, ErrNoPlacement
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			return candidates[i].Pool.ID < candidates[j].Pool.ID
		}
		return candidates[i].Score > candidates[j].Score
	})
	result.Placement = candidates[0]
	sort.Slice(result.Rejected, func(i, j int) bool { return result.Rejected[i].PoolID < result.Rejected[j].PoolID })
	return result, nil
}

func score(spec workload.Spec, pool RuntimePool) []ScoreComponent {
	preferred := 0.0
	if pool.RuntimeClass == spec.Placement.PreferredClass {
		preferred = 30
	}
	locality := 0.0
	if spec.Placement.ArtifactRegion != "" && slices.Contains(pool.ArtifactRegions, spec.Placement.ArtifactRegion) {
		locality = 20
	}
	cpuHeadroom := ratio(pool.AvailableCPU-spec.Placement.CPU, pool.AvailableCPU) * 20
	memoryHeadroom := ratio(pool.AvailableMemory-spec.Placement.Memory, pool.AvailableMemory) * 15
	llmHeadroom := ratio(int64(pool.AvailableLLMSlots-spec.Placement.LLMConcurrency), int64(pool.AvailableLLMSlots)) * 15
	cost := -math.Max(0, pool.CostWeight) * 10
	return []ScoreComponent{
		{Name: "preferredRuntimeClass", Value: preferred, Reason: "preferred runtime class match"},
		{Name: "artifactLocality", Value: locality, Reason: "artifact region locality"},
		{Name: "cpuHeadroom", Value: cpuHeadroom, Reason: "remaining CPU capacity"},
		{Name: "memoryHeadroom", Value: memoryHeadroom, Reason: "remaining memory capacity"},
		{Name: "llmHeadroom", Value: llmHeadroom, Reason: "remaining LLM concurrency"},
		{Name: "cost", Value: cost, Reason: "normalized runtime cost penalty"},
	}
}

func ratio(numerator, denominator int64) float64 {
	if denominator <= 0 || numerator <= 0 {
		return 0
	}
	return math.Min(1, float64(numerator)/float64(denominator))
}

type PoolSource interface {
	ListRuntimePools(context.Context, string) ([]RuntimePool, error)
}

type StaticPoolSource []RuntimePool

func (s StaticPoolSource) ListRuntimePools(_ context.Context, tenantID string) ([]RuntimePool, error) {
	var pools []RuntimePool
	for _, pool := range s {
		if slices.Contains(pool.TenantIDs, tenantID) {
			pools = append(pools, pool)
		}
	}
	return pools, nil
}

// LeaseAwarePoolSource overlays lease-derived runtime health onto a static
// pool configuration (v0.6): a pool is ready only when its static
// configuration says ready AND its runtime instance is presumed alive by
// recent lease heartbeats. Placement therefore rejects pools whose worker
// stopped renewing its lease instead of scheduling a task that would be
// stranded until lease-expiry recovery. An instance ID missing from the
// health result is treated as unhealthy (fail-closed).
type LeaseAwarePoolSource struct {
	static    PoolSource
	health    store.PoolHealthStore
	freshness time.Duration
}

func NewLeaseAwarePoolSource(static PoolSource, health store.PoolHealthStore, freshness time.Duration) *LeaseAwarePoolSource {
	return &LeaseAwarePoolSource{static: static, health: health, freshness: freshness}
}

func (s *LeaseAwarePoolSource) ListRuntimePools(ctx context.Context, tenantID string) ([]RuntimePool, error) {
	pools, err := s.static.ListRuntimePools(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var instanceIDs []string
	for _, pool := range pools {
		if pool.Ready {
			instanceIDs = append(instanceIDs, pool.RuntimeInstanceID)
		}
	}
	health, err := s.health.PoolInstanceHealth(ctx, instanceIDs, time.Now().UTC(), s.freshness)
	if err != nil {
		return nil, err
	}
	for i := range pools {
		if !pools[i].Ready {
			continue
		}
		alive, ok := health[pools[i].RuntimeInstanceID]
		if !ok || !alive {
			pools[i].Ready = false
		}
	}
	return pools, nil
}

type Controller struct {
	store    store.ControlStore
	pools    PoolSource
	ownerID  string
	batch    int
	claimTTL time.Duration
	leaseTTL time.Duration
	newID    func() uuid.UUID
	// parallel bounds concurrent per-task processing within one batch (P1);
	// 1 disables parallelism.
	parallel int
}

func NewController(repository store.ControlStore, pools PoolSource, ownerID string, batch int, claimTTL, leaseTTL time.Duration) *Controller {
	return &Controller{store: repository, pools: pools, ownerID: ownerID, batch: batch,
		claimTTL: claimTTL, leaseTTL: leaseTTL, newID: newUUIDv7, parallel: 4}
}

// WithParallelism bounds concurrent per-task processing within one reconcile
// batch (default 4; 1 = serial).
func WithParallelism(workers int) func(*Controller) {
	return func(c *Controller) {
		if workers > 0 {
			c.parallel = workers
		}
	}
}

// Reconcile claims and schedules admitted tasks. Transient transaction
// conflicts are retried with bounded backoff (ADR-002).
func (c *Controller) Reconcile(ctx context.Context) (int, error) {
	return store.RetryRetryable(ctx, func() (int, error) { return c.reconcileOnce(ctx) })
}

func (c *Controller) reconcileOnce(ctx context.Context) (int, error) {
	claims, err := c.store.ClaimTasks(ctx, store.ClaimTasksInput{
		Kind: store.ControllerScheduling, Phase: "ADMITTED", OwnerID: c.ownerID, Limit: c.batch, TTL: c.claimTTL,
	})
	if err != nil {
		return 0, err
	}
	return c.processClaims(ctx, claims)
}

func (c *Controller) processClaims(ctx context.Context, claims []store.TaskClaim) (int, error) {
	if c.parallel <= 1 || len(claims) <= 1 {
		processed := 0
		for _, claim := range claims {
			ok, err := c.processClaim(ctx, claim)
			if err != nil {
				return processed, err
			}
			if ok {
				processed++
			}
		}
		return processed, nil
	}
	processed := 0
	var mu sync.Mutex
	var batchErr error
	semaphore := make(chan struct{}, c.parallel)
	var wg sync.WaitGroup
	for _, claim := range claims {
		claim := claim
		mu.Lock()
		aborted := batchErr != nil
		mu.Unlock()
		if aborted {
			break
		}
		wg.Add(1)
		semaphore <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			ok, err := c.processClaim(ctx, claim)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if batchErr == nil {
					batchErr = err
				}
				return
			}
			if ok {
				processed++
			}
		}()
	}
	wg.Wait()
	return processed, batchErr
}

// processClaim runs one claim through placement, returning whether the claim
// was scheduled. Non-retryable failures are isolated per claim; retryable
// failures abort the batch.
func (c *Controller) processClaim(ctx context.Context, claim store.TaskClaim) (bool, error) {
	spec, err := workload.Decode(claim.Task.Spec)
	if err != nil {
		if store.IsRetryableTransaction(err) {
			return false, err
		}
		// Per-task error isolation: an undecodable admitted task must not
		// block the rest of the batch. The claim is released so the task
		// is re-evaluated (or handled by lifecycle timeout) later.
		_ = c.store.ReleaseTaskClaim(ctx, claim)
		slog.Error("schedule decode failed for task; isolated", "task", claim.Task.ID, "tenant", claim.Task.TenantID, "error", err)
		return false, nil
	}
	pools, err := c.pools.ListRuntimePools(ctx, claim.Task.TenantID)
	if err != nil {
		if store.IsRetryableTransaction(err) {
			return false, err
		}
		_ = c.store.ReleaseTaskClaim(ctx, claim)
		slog.Error("runtime pool list failed for task; isolated", "task", claim.Task.ID, "tenant", claim.Task.TenantID, "error", err)
		return false, nil
	}
	selection, err := Select(spec, pools)
	if err != nil {
		if errors.Is(err, ErrNoPlacement) {
			// O6: release the claim immediately and defer the next attempt
			// with exponential backoff instead of pinning the claim until
			// its TTL. The deferral lives on the task, so every controller
			// instance honors the same backoff and the task is claimable
			// again the moment it elapses.
			_, deferErr := c.store.DeferTaskSchedule(ctx, store.DeferTaskScheduleInput{
				TaskID: claim.Task.ID, TenantID: claim.Task.TenantID, OwnerID: claim.OwnerID,
				ClaimFencingToken: claim.FencingToken, ExpectedTaskVersion: claim.Task.ResourceVersion,
				Until: time.Now().UTC().Add(scheduleBackoff(claim.Task.ScheduleRetryCount)),
			})
			if deferErr != nil {
				if store.IsRetryableTransaction(deferErr) {
					return false, deferErr
				}
				// The deferral commit failed (stale claim or unexpected
				// store failure): release the claim so the task is not
				// pinned, and continue.
				_ = c.store.ReleaseTaskClaim(ctx, claim)
				slog.Error("schedule deferral failed for task; isolated", "task", claim.Task.ID, "tenant", claim.Task.TenantID, "error", deferErr)
			}
			return false, nil
		}
		_ = c.store.ReleaseTaskClaim(ctx, claim)
		slog.Error("placement selection failed for task; isolated", "task", claim.Task.ID, "tenant", claim.Task.TenantID, "error", err)
		return false, nil
	}
	pool := selection.Placement.Pool
	_, err = c.store.ScheduleTask(ctx, store.ScheduleTaskInput{
		TaskID: claim.Task.ID, TenantID: claim.Task.TenantID, OwnerID: claim.OwnerID,
		ClaimFencingToken: claim.FencingToken, ExpectedTaskVersion: claim.Task.ResourceVersion,
		RunID: c.newID(), AttemptID: c.newID(), LeaseID: c.newID(), RuntimePoolID: pool.ID,
		RuntimeClass: pool.RuntimeClass, RuntimeInstanceID: pool.RuntimeInstanceID, LeaseTTL: c.leaseTTL,
	})
	if err != nil {
		if store.IsRetryableTransaction(err) {
			return false, err
		}
		// A stale claim or a per-task schedule failure: release and
		// continue so one task cannot starve the batch.
		_ = c.store.ReleaseTaskClaim(ctx, claim)
		slog.Error("schedule commit failed for task; isolated", "task", claim.Task.ID, "tenant", claim.Task.TenantID, "error", err)
		return false, nil
	}
	return true, nil
}

func newUUIDv7() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}

// scheduleBackoff returns the exponential backoff for a task's next
// scheduling attempt after a no-placement (O6): 5s doubled per consecutive
// deferral, capped at 5 minutes. The retry count comes from the task row, so
// the progression survives controller restarts and is shared across
// instances.
func scheduleBackoff(retries int64) time.Duration {
	const (
		base = 5 * time.Second
		max  = 5 * time.Minute
	)
	if retries < 0 {
		retries = 0
	}
	if retries > 6 {
		return max
	}
	backoff := base << retries
	if backoff > max {
		return max
	}
	return backoff
}
