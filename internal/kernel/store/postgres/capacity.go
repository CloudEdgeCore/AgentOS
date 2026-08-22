package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	kernelstore "github.com/CloudEdgeCore/AgentOS/internal/kernel/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// reserveRuntimeCapacity serializes every placement for a pool on its
// capacity row and commits the reservation with Task/Run/Attempt/Lease.
func (s *Store) reserveRuntimeCapacity(ctx context.Context, tx pgx.Tx, in kernelstore.ScheduleTaskInput, now time.Time) error {
	command, err := tx.Exec(ctx, `INSERT INTO runtime_pool_capacities (
		pool_id, total_cpu_millis, total_memory_mib, total_llm_slots, updated_at
	) VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (pool_id) DO UPDATE SET
		total_cpu_millis = EXCLUDED.total_cpu_millis,
		total_memory_mib = EXCLUDED.total_memory_mib,
		total_llm_slots = EXCLUDED.total_llm_slots,
		resource_version = runtime_pool_capacities.resource_version + 1,
		updated_at = EXCLUDED.updated_at
	WHERE runtime_pool_capacities.reserved_cpu_millis <= EXCLUDED.total_cpu_millis
	  AND runtime_pool_capacities.reserved_memory_mib <= EXCLUDED.total_memory_mib
	  AND runtime_pool_capacities.reserved_llm_slots <= EXCLUDED.total_llm_slots`,
		in.RuntimePoolID, in.PoolCPUCapacity, in.PoolMemoryCapacity, in.PoolLLMCapacity, now)
	if err != nil {
		return classify(err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: pool %s cannot shrink below active reservations", kernelstore.ErrCapacityExhausted, in.RuntimePoolID)
	}

	command, err = tx.Exec(ctx, `UPDATE runtime_pool_capacities SET
		reserved_cpu_millis = reserved_cpu_millis + $1,
		reserved_memory_mib = reserved_memory_mib + $2,
		reserved_llm_slots = reserved_llm_slots + $3,
		resource_version = resource_version + 1, updated_at = $4
	WHERE pool_id = $5
	  AND reserved_cpu_millis + $1 <= total_cpu_millis
	  AND reserved_memory_mib + $2 <= total_memory_mib
	  AND reserved_llm_slots + $3 <= total_llm_slots`,
		in.RequestedCPU, in.RequestedMemory, in.RequestedLLMSlots, now, in.RuntimePoolID)
	if err != nil {
		return classify(err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("%w: pool=%s cpu=%d memory=%d llm=%d", kernelstore.ErrCapacityExhausted,
			in.RuntimePoolID, in.RequestedCPU, in.RequestedMemory, in.RequestedLLMSlots)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runtime_capacity_reservations (
		tenant_id, task_id, pool_id, cpu_millis, memory_mib, llm_slots,
		owner_fencing_token, status, created_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE', $8)`,
		in.TenantID, in.TaskID.String(), in.RuntimePoolID, in.RequestedCPU,
		in.RequestedMemory, in.RequestedLLMSlots, in.ClaimFencingToken, now); err != nil {
		return classify(err)
	}
	return nil
}

// releaseRuntimeCapacity is idempotent and task-scoped. The reservation row
// is locked before decrementing the pool, so stale completion retries cannot
// release capacity owned by another task or twice.
func (s *Store) releaseRuntimeCapacity(ctx context.Context, tx pgx.Tx, tenantID string, taskID uuid.UUID, now time.Time) error {
	var poolID string
	var cpu, memory int64
	var llm int
	err := tx.QueryRow(ctx, `SELECT pool_id, cpu_millis, memory_mib, llm_slots
		FROM runtime_capacity_reservations
		WHERE tenant_id = $1 AND task_id = $2 AND status = 'ACTIVE' FOR UPDATE`,
		tenantID, taskID.String()).Scan(&poolID, &cpu, &memory, &llm)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return classify(err)
	}
	command, err := tx.Exec(ctx, `UPDATE runtime_pool_capacities SET
		reserved_cpu_millis = reserved_cpu_millis - $1,
		reserved_memory_mib = reserved_memory_mib - $2,
		reserved_llm_slots = reserved_llm_slots - $3,
		resource_version = resource_version + 1, updated_at = $4
	WHERE pool_id = $5
	  AND reserved_cpu_millis >= $1
	  AND reserved_memory_mib >= $2
	  AND reserved_llm_slots >= $3`, cpu, memory, llm, now, poolID)
	if err != nil {
		return classify(err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("capacity ledger drift for pool %s", poolID)
	}
	command, err = tx.Exec(ctx, `UPDATE runtime_capacity_reservations
		SET status = 'RELEASED', released_at = $1
		WHERE tenant_id = $2 AND task_id = $3 AND status = 'ACTIVE'`, now, tenantID, taskID.String())
	if err != nil {
		return classify(err)
	}
	if command.RowsAffected() != 1 {
		return fmt.Errorf("capacity reservation changed before release for task %s", taskID)
	}
	return nil
}
