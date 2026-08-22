package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/scheduler"
	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
)

// RegisterRuntimePools upserts development/bootstrap pool declarations into
// the durable registry. Tenant grants are replaced per named pool atomically;
// unrelated pools are never deleted.
func (s *Store) RegisterRuntimePools(ctx context.Context, pools []scheduler.RuntimePool) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	now := s.now()
	seen := map[string]struct{}{}
	for _, pool := range pools {
		if strings.TrimSpace(pool.ID) == "" || strings.TrimSpace(pool.RuntimeClass) == "" ||
			strings.TrimSpace(pool.RuntimeInstanceID) == "" || strings.TrimSpace(pool.Region) == "" ||
			len(pool.TenantIDs) == 0 || pool.AvailableCPU < 0 || pool.AvailableMemory < 0 || pool.AvailableLLMSlots < 0 ||
			pool.CostWeight < 0 || math.IsNaN(pool.CostWeight) || math.IsInf(pool.CostWeight, 0) {
			return fmt.Errorf("runtime pool %q is invalid", pool.ID)
		}
		if _, duplicate := seen[pool.ID]; duplicate {
			return fmt.Errorf("duplicate runtime pool %q", pool.ID)
		}
		seen[pool.ID] = struct{}{}
		status := strings.ToUpper(strings.TrimSpace(pool.Status))
		if status == "" {
			status = "ACTIVE"
		}
		if status != "ACTIVE" && status != "CORDONED" && status != "DRAINING" {
			return fmt.Errorf("runtime pool %q status is invalid", pool.ID)
		}
		regions, err := json.Marshal(pool.ArtifactRegions)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO runtime_pools (
			id,runtime_class,runtime_instance_id,region,data_residency,ready,status,failure_domain,
			available_cpu_millis,available_memory_mib,available_llm_slots,artifact_regions,cost_weight,created_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14)
			ON CONFLICT (id) DO UPDATE SET runtime_class=EXCLUDED.runtime_class,
			runtime_instance_id=EXCLUDED.runtime_instance_id,region=EXCLUDED.region,
			data_residency=EXCLUDED.data_residency,ready=EXCLUDED.ready,status=EXCLUDED.status,
			failure_domain=EXCLUDED.failure_domain,available_cpu_millis=EXCLUDED.available_cpu_millis,
			available_memory_mib=EXCLUDED.available_memory_mib,available_llm_slots=EXCLUDED.available_llm_slots,
			artifact_regions=EXCLUDED.artifact_regions,cost_weight=EXCLUDED.cost_weight,
			resource_version=runtime_pools.resource_version+1,updated_at=EXCLUDED.updated_at`,
			pool.ID, pool.RuntimeClass, pool.RuntimeInstanceID, pool.Region, pool.DataResidency, pool.Ready,
			status, pool.FailureDomain, pool.AvailableCPU, pool.AvailableMemory, pool.AvailableLLMSlots,
			regions, pool.CostWeight, now); err != nil {
			return classify(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM runtime_pool_tenant_grants WHERE pool_id=$1`, pool.ID); err != nil {
			return classify(err)
		}
		tenants := map[string]struct{}{}
		for _, tenantID := range pool.TenantIDs {
			if strings.TrimSpace(tenantID) == "" {
				return fmt.Errorf("runtime pool %q has an empty tenant grant", pool.ID)
			}
			if _, duplicate := tenants[tenantID]; duplicate {
				continue
			}
			tenants[tenantID] = struct{}{}
			if _, err := tx.Exec(ctx, `INSERT INTO runtime_pool_tenant_grants(pool_id,tenant_id,created_at)
				VALUES($1,$2,$3)`, pool.ID, tenantID, now); err != nil {
				return classify(err)
			}
		}
	}
	return classify(tx.Commit(ctx))
}

// UpdateRuntimePoolStatus performs a tenant-authorized, CAS-guarded operator
// transition. Cordon/drain affect new placement only; existing leases remain
// recoverable until their normal terminal path.
func (s *Store) UpdateRuntimePoolStatus(ctx context.Context, in kernelstore.UpdateRuntimePoolStatusInput) (kernelstore.RuntimePoolState, error) {
	var state kernelstore.RuntimePoolState
	status := strings.ToUpper(strings.TrimSpace(in.Status))
	if strings.TrimSpace(in.TenantID) == "" || strings.TrimSpace(in.PoolID) == "" || in.ExpectedVersion <= 0 ||
		(status != "ACTIVE" && status != "CORDONED" && status != "DRAINING") {
		return state, fmt.Errorf("tenant, pool, positive expected version, and ACTIVE|CORDONED|DRAINING status are required")
	}
	err := s.pool.QueryRow(ctx, `UPDATE runtime_pools p SET status=$1,resource_version=p.resource_version+1,updated_at=$2
		WHERE p.id=$3 AND p.resource_version=$4 AND EXISTS(
			SELECT 1 FROM runtime_pool_tenant_grants g WHERE g.pool_id=p.id AND g.tenant_id=$5)
		RETURNING p.id,p.status,p.resource_version`, status, s.now(), in.PoolID, in.ExpectedVersion, in.TenantID).
		Scan(&state.ID, &state.Status, &state.ResourceVersion)
	if err != nil {
		return state, classify(err)
	}
	return state, nil
}

// ListRuntimePools resolves tenant authorization dynamically from the
// registry and never accepts an implicit/global tenant grant.
func (s *Store) ListRuntimePools(ctx context.Context, tenantID string) ([]scheduler.RuntimePool, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("tenant is required")
	}
	rows, err := s.pool.Query(ctx, `SELECT p.id,p.runtime_class,p.runtime_instance_id,p.region,p.data_residency,
		p.ready,p.status,p.failure_domain,p.available_cpu_millis,p.available_memory_mib,
		p.available_llm_slots,p.artifact_regions,p.cost_weight
		FROM runtime_pools p JOIN runtime_pool_tenant_grants g ON g.pool_id=p.id
		WHERE g.tenant_id=$1 ORDER BY p.id`, tenantID)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	var pools []scheduler.RuntimePool
	for rows.Next() {
		var pool scheduler.RuntimePool
		var regions []byte
		if err := rows.Scan(&pool.ID, &pool.RuntimeClass, &pool.RuntimeInstanceID, &pool.Region,
			&pool.DataResidency, &pool.Ready, &pool.Status, &pool.FailureDomain, &pool.AvailableCPU,
			&pool.AvailableMemory, &pool.AvailableLLMSlots, &regions, &pool.CostWeight); err != nil {
			return nil, classify(err)
		}
		if err := json.Unmarshal(regions, &pool.ArtifactRegions); err != nil {
			return nil, fmt.Errorf("decode runtime pool %q artifact regions: %w", pool.ID, err)
		}
		pool.TenantIDs = []string{tenantID}
		pools = append(pools, pool)
	}
	return pools, classify(rows.Err())
}
