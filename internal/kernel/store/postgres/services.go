package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/supervisor"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ supervisor.Store = (*Store)(nil)

// CreateService inserts a new agent service record.
func (s *Store) CreateService(ctx context.Context, svc *supervisor.Service) error {
	if svc == nil {
		return supervisor.ErrInvalidServiceSpec
	}
	specBytes, err := json.Marshal(svc.Spec)
	if err != nil {
		return fmt.Errorf("marshal service spec: %w", err)
	}
	statusBytes, err := json.Marshal(svc.Status)
	if err != nil {
		return fmt.Errorf("marshal service status: %w", err)
	}

	now := s.clock()
	if svc.CreatedAt.IsZero() {
		svc.CreatedAt = now
	}
	svc.UpdatedAt = now

	const query = `
		INSERT INTO agent_services (id, tenant_id, namespace, name, agent_id, spec, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err = s.pool.Exec(ctx, query,
		svc.ID, svc.TenantID, svc.Namespace, svc.Name, svc.AgentID,
		specBytes, statusBytes, svc.CreatedAt, svc.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return supervisor.ErrServiceAlreadyExists
		}
		return err
	}
	return nil
}

// GetService retrieves an agent service by tenant and ID.
func (s *Store) GetService(ctx context.Context, tenantID, serviceID string) (*supervisor.Service, error) {
	const query = `
		SELECT id, tenant_id, namespace, name, agent_id, spec, status, created_at, updated_at
		FROM agent_services
		WHERE tenant_id = $1 AND id = $2
	`
	var (
		svc         supervisor.Service
		specBytes   []byte
		statusBytes []byte
	)
	err := s.pool.QueryRow(ctx, query, tenantID, serviceID).Scan(
		&svc.ID, &svc.TenantID, &svc.Namespace, &svc.Name, &svc.AgentID,
		&specBytes, &statusBytes, &svc.CreatedAt, &svc.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, supervisor.ErrServiceNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(specBytes, &svc.Spec); err != nil {
		return nil, fmt.Errorf("unmarshal spec: %w", err)
	}
	if err := json.Unmarshal(statusBytes, &svc.Status); err != nil {
		return nil, fmt.Errorf("unmarshal status: %w", err)
	}
	return &svc, nil
}

// GetServiceByName retrieves an agent service by tenant, namespace, and name.
func (s *Store) GetServiceByName(ctx context.Context, tenantID, namespace, name string) (*supervisor.Service, error) {
	const query = `
		SELECT id, tenant_id, namespace, name, agent_id, spec, status, created_at, updated_at
		FROM agent_services
		WHERE tenant_id = $1 AND namespace = $2 AND name = $3
	`
	var (
		svc         supervisor.Service
		specBytes   []byte
		statusBytes []byte
	)
	err := s.pool.QueryRow(ctx, query, tenantID, namespace, name).Scan(
		&svc.ID, &svc.TenantID, &svc.Namespace, &svc.Name, &svc.AgentID,
		&specBytes, &statusBytes, &svc.CreatedAt, &svc.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, supervisor.ErrServiceNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(specBytes, &svc.Spec); err != nil {
		return nil, fmt.Errorf("unmarshal spec: %w", err)
	}
	if err := json.Unmarshal(statusBytes, &svc.Status); err != nil {
		return nil, fmt.Errorf("unmarshal status: %w", err)
	}
	return &svc, nil
}

// UpdateService updates spec, status, and updatedAt for a service.
func (s *Store) UpdateService(ctx context.Context, svc *supervisor.Service) error {
	if svc == nil {
		return supervisor.ErrInvalidServiceSpec
	}
	specBytes, err := json.Marshal(svc.Spec)
	if err != nil {
		return fmt.Errorf("marshal service spec: %w", err)
	}
	statusBytes, err := json.Marshal(svc.Status)
	if err != nil {
		return fmt.Errorf("marshal service status: %w", err)
	}

	svc.UpdatedAt = s.clock()

	const query = `
		UPDATE agent_services
		SET name = $1, agent_id = $2, spec = $3, status = $4, updated_at = $5
		WHERE tenant_id = $6 AND id = $7
	`
	tag, err := s.pool.Exec(ctx, query,
		svc.Name, svc.AgentID, specBytes, statusBytes, svc.UpdatedAt,
		svc.TenantID, svc.ID,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return supervisor.ErrServiceAlreadyExists
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return supervisor.ErrServiceNotFound
	}
	return nil
}

// DeleteService removes a service by tenant and ID.
func (s *Store) DeleteService(ctx context.Context, tenantID, serviceID string) error {
	const query = `
		DELETE FROM agent_services
		WHERE tenant_id = $1 AND id = $2
	`
	tag, err := s.pool.Exec(ctx, query, tenantID, serviceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return supervisor.ErrServiceNotFound
	}
	return nil
}

// ListServices lists services within a tenant, optionally filtered by namespace.
func (s *Store) ListServices(ctx context.Context, tenantID, namespace string) ([]*supervisor.Service, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if namespace != "" {
		const query = `
			SELECT id, tenant_id, namespace, name, agent_id, spec, status, created_at, updated_at
			FROM agent_services
			WHERE tenant_id = $1 AND namespace = $2
			ORDER BY created_at ASC
		`
		rows, err = s.pool.Query(ctx, query, tenantID, namespace)
	} else {
		const query = `
			SELECT id, tenant_id, namespace, name, agent_id, spec, status, created_at, updated_at
			FROM agent_services
			WHERE tenant_id = $1
			ORDER BY created_at ASC
		`
		rows, err = s.pool.Query(ctx, query, tenantID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*supervisor.Service
	for rows.Next() {
		var (
			svc         supervisor.Service
			specBytes   []byte
			statusBytes []byte
		)
		if err := rows.Scan(
			&svc.ID, &svc.TenantID, &svc.Namespace, &svc.Name, &svc.AgentID,
			&specBytes, &statusBytes, &svc.CreatedAt, &svc.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(specBytes, &svc.Spec); err != nil {
			return nil, fmt.Errorf("unmarshal spec: %w", err)
		}
		if err := json.Unmarshal(statusBytes, &svc.Status); err != nil {
			return nil, fmt.Errorf("unmarshal status: %w", err)
		}
		result = append(result, &svc)
	}
	return result, rows.Err()
}

// CreateInstance inserts a new service instance record.
func (s *Store) CreateInstance(ctx context.Context, inst *supervisor.Instance) error {
	if inst == nil {
		return supervisor.ErrInstanceNotFound
	}
	addressBytes, err := json.Marshal(inst.Address)
	if err != nil {
		return fmt.Errorf("marshal address: %w", err)
	}

	now := s.clock()
	if inst.CreatedAt.IsZero() {
		inst.CreatedAt = now
	}
	inst.UpdatedAt = now
	if inst.LastHeartbeat.IsZero() {
		inst.LastHeartbeat = now
	}

	const query = `
		INSERT INTO agent_service_instances (
			id, service_id, tenant_id, namespace, agent_id, address,
			phase, restart_count, consecutive_failures, next_restart_at,
			last_heartbeat, created_at, updated_at, terminated_at, exit_code, exit_reason
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	`
	_, err = s.pool.Exec(ctx, query,
		inst.ID, inst.ServiceID, inst.TenantID, inst.Namespace, inst.AgentID, addressBytes,
		string(inst.Phase), inst.RestartCount, inst.ConsecutiveFailures, inst.NextRestartAt,
		inst.LastHeartbeat, inst.CreatedAt, inst.UpdatedAt, inst.TerminatedAt, inst.ExitCode, inst.ExitReason,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return supervisor.ErrInstanceAlreadyExists
		}
		return err
	}
	return nil
}

// GetInstance retrieves an instance by tenant, service ID, and instance ID.
func (s *Store) GetInstance(ctx context.Context, tenantID, serviceID, instanceID string) (*supervisor.Instance, error) {
	const query = `
		SELECT id, service_id, tenant_id, namespace, agent_id, address,
		       phase, restart_count, consecutive_failures, next_restart_at,
		       last_heartbeat, created_at, updated_at, terminated_at, exit_code, exit_reason
		FROM agent_service_instances
		WHERE tenant_id = $1 AND service_id = $2 AND id = $3
	`
	var (
		inst         supervisor.Instance
		addressBytes []byte
		phaseStr     string
	)
	err := s.pool.QueryRow(ctx, query, tenantID, serviceID, instanceID).Scan(
		&inst.ID, &inst.ServiceID, &inst.TenantID, &inst.Namespace, &inst.AgentID, &addressBytes,
		&phaseStr, &inst.RestartCount, &inst.ConsecutiveFailures, &inst.NextRestartAt,
		&inst.LastHeartbeat, &inst.CreatedAt, &inst.UpdatedAt, &inst.TerminatedAt, &inst.ExitCode, &inst.ExitReason,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, supervisor.ErrInstanceNotFound
		}
		return nil, err
	}
	inst.Phase = supervisor.InstancePhase(phaseStr)
	if err := json.Unmarshal(addressBytes, &inst.Address); err != nil {
		return nil, fmt.Errorf("unmarshal address: %w", err)
	}
	return &inst, nil
}

// UpdateInstance updates an instance record.
func (s *Store) UpdateInstance(ctx context.Context, inst *supervisor.Instance) error {
	if inst == nil {
		return supervisor.ErrInstanceNotFound
	}
	addressBytes, err := json.Marshal(inst.Address)
	if err != nil {
		return fmt.Errorf("marshal address: %w", err)
	}

	inst.UpdatedAt = s.clock()

	const query = `
		UPDATE agent_service_instances
		SET address = $1, phase = $2, restart_count = $3, consecutive_failures = $4,
		    next_restart_at = $5, last_heartbeat = $6, updated_at = $7,
		    terminated_at = $8, exit_code = $9, exit_reason = $10
		WHERE tenant_id = $11 AND service_id = $12 AND id = $13
	`
	tag, err := s.pool.Exec(ctx, query,
		addressBytes, string(inst.Phase), inst.RestartCount, inst.ConsecutiveFailures,
		inst.NextRestartAt, inst.LastHeartbeat, inst.UpdatedAt,
		inst.TerminatedAt, inst.ExitCode, inst.ExitReason,
		inst.TenantID, inst.ServiceID, inst.ID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return supervisor.ErrInstanceNotFound
	}
	return nil
}

// DeleteInstance deletes an instance record.
func (s *Store) DeleteInstance(ctx context.Context, tenantID, serviceID, instanceID string) error {
	const query = `
		DELETE FROM agent_service_instances
		WHERE tenant_id = $1 AND service_id = $2 AND id = $3
	`
	tag, err := s.pool.Exec(ctx, query, tenantID, serviceID, instanceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return supervisor.ErrInstanceNotFound
	}
	return nil
}

// ListInstances lists all instances belonging to a service.
func (s *Store) ListInstances(ctx context.Context, tenantID, serviceID string) ([]*supervisor.Instance, error) {
	const query = `
		SELECT id, service_id, tenant_id, namespace, agent_id, address,
		       phase, restart_count, consecutive_failures, next_restart_at,
		       last_heartbeat, created_at, updated_at, terminated_at, exit_code, exit_reason
		FROM agent_service_instances
		WHERE tenant_id = $1 AND service_id = $2
		ORDER BY created_at ASC
	`
	rows, err := s.pool.Query(ctx, query, tenantID, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*supervisor.Instance
	for rows.Next() {
		var (
			inst         supervisor.Instance
			addressBytes []byte
			phaseStr     string
		)
		if err := rows.Scan(
			&inst.ID, &inst.ServiceID, &inst.TenantID, &inst.Namespace, &inst.AgentID, &addressBytes,
			&phaseStr, &inst.RestartCount, &inst.ConsecutiveFailures, &inst.NextRestartAt,
			&inst.LastHeartbeat, &inst.CreatedAt, &inst.UpdatedAt, &inst.TerminatedAt, &inst.ExitCode, &inst.ExitReason,
		); err != nil {
			return nil, err
		}
		inst.Phase = supervisor.InstancePhase(phaseStr)
		if err := json.Unmarshal(addressBytes, &inst.Address); err != nil {
			return nil, fmt.Errorf("unmarshal address: %w", err)
		}
		result = append(result, &inst)
	}
	return result, rows.Err()
}

// ListAllInstances lists all instances for a tenant.
func (s *Store) ListAllInstances(ctx context.Context, tenantID string) ([]*supervisor.Instance, error) {
	const query = `
		SELECT id, service_id, tenant_id, namespace, agent_id, address,
		       phase, restart_count, consecutive_failures, next_restart_at,
		       last_heartbeat, created_at, updated_at, terminated_at, exit_code, exit_reason
		FROM agent_service_instances
		WHERE tenant_id = $1
		ORDER BY created_at ASC
	`
	rows, err := s.pool.Query(ctx, query, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*supervisor.Instance
	for rows.Next() {
		var (
			inst         supervisor.Instance
			addressBytes []byte
			phaseStr     string
		)
		if err := rows.Scan(
			&inst.ID, &inst.ServiceID, &inst.TenantID, &inst.Namespace, &inst.AgentID, &addressBytes,
			&phaseStr, &inst.RestartCount, &inst.ConsecutiveFailures, &inst.NextRestartAt,
			&inst.LastHeartbeat, &inst.CreatedAt, &inst.UpdatedAt, &inst.TerminatedAt, &inst.ExitCode, &inst.ExitReason,
		); err != nil {
			return nil, err
		}
		inst.Phase = supervisor.InstancePhase(phaseStr)
		if err := json.Unmarshal(addressBytes, &inst.Address); err != nil {
			return nil, fmt.Errorf("unmarshal address: %w", err)
		}
		result = append(result, &inst)
	}
	return result, rows.Err()
}
