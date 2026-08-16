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

type Controller struct {
	store    store.ControlStore
	pools    PoolSource
	ownerID  string
	batch    int
	claimTTL time.Duration
	leaseTTL time.Duration
	newID    func() uuid.UUID
}

func NewController(repository store.ControlStore, pools PoolSource, ownerID string, batch int, claimTTL, leaseTTL time.Duration) *Controller {
	return &Controller{store: repository, pools: pools, ownerID: ownerID, batch: batch,
		claimTTL: claimTTL, leaseTTL: leaseTTL, newID: newUUIDv7}
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
	processed := 0
	for _, claim := range claims {
		spec, err := workload.Decode(claim.Task.Spec)
		if err != nil {
			if store.IsRetryableTransaction(err) {
				return processed, err
			}
			// Per-task error isolation: an undecodable admitted task must not
			// block the rest of the batch. The claim is released so the task
			// is re-evaluated (or handled by lifecycle timeout) later.
			_ = c.store.ReleaseTaskClaim(ctx, claim)
			slog.Error("schedule decode failed for task; isolated", "task", claim.Task.ID, "tenant", claim.Task.TenantID, "error", err)
			continue
		}
		pools, err := c.pools.ListRuntimePools(ctx, claim.Task.TenantID)
		if err != nil {
			if store.IsRetryableTransaction(err) {
				return processed, err
			}
			_ = c.store.ReleaseTaskClaim(ctx, claim)
			slog.Error("runtime pool list failed for task; isolated", "task", claim.Task.ID, "tenant", claim.Task.TenantID, "error", err)
			continue
		}
		selection, err := Select(spec, pools)
		if err != nil {
			if errors.Is(err, ErrNoPlacement) {
				// Keep the short claim until expiry to avoid hot-looping when all
				// pools are full. Another controller may retry after the TTL.
				continue
			}
			_ = c.store.ReleaseTaskClaim(ctx, claim)
			slog.Error("placement selection failed for task; isolated", "task", claim.Task.ID, "tenant", claim.Task.TenantID, "error", err)
			continue
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
				return processed, err
			}
			// A stale claim or a per-task schedule failure: release and
			// continue so one task cannot starve the batch.
			_ = c.store.ReleaseTaskClaim(ctx, claim)
			slog.Error("schedule commit failed for task; isolated", "task", claim.Task.ID, "tenant", claim.Task.TenantID, "error", err)
			continue
		}
		processed++
	}
	return processed, nil
}

func newUUIDv7() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}
