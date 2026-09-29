package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/CloudEdgeCore/AgentOS/internal/kernel/ipc"
	"github.com/google/uuid"
)

// Option is a functional option for configuring the Supervisor.
type Option func(*Supervisor)

// WithClock sets a custom clock for deterministic testing.
func WithClock(clock func() time.Time) Option {
	return func(s *Supervisor) {
		s.clock = clock
	}
}

// WithSpawner sets the instance spawner implementation.
func WithSpawner(spawner InstanceSpawner) Option {
	return func(s *Supervisor) {
		s.spawner = spawner
	}
}

// WithIPCService attaches the kernel IPC service for mailbox awareness and autowake.
func WithIPCService(ipcSvc *ipc.Service) Option {
	return func(s *Supervisor) {
		s.ipcSvc = ipcSvc
	}
}

// WithMailbox attaches a Mailbox for inspecting pending messages on autowake.
func WithMailbox(mailbox ipc.Mailbox) Option {
	return func(s *Supervisor) {
		s.mailbox = mailbox
	}
}

// Supervisor manages long-running Agent Services, monitors instance health,
// handles crash recovery with exponential backoffs, and balances replica counts.
type Supervisor struct {
	mu      sync.Mutex
	store   Store
	spawner InstanceSpawner
	ipcSvc  *ipc.Service
	mailbox ipc.Mailbox
	clock   func() time.Time
	idGen   func() string
}

// NewSupervisor constructs a new Supervisor engine.
func NewSupervisor(store Store, opts ...Option) *Supervisor {
	s := &Supervisor{
		store:   store,
		spawner: NewMockSpawner(),
		clock: func() time.Time {
			return time.Now().UTC()
		},
		idGen: func() string {
			return uuid.New().String()
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateService registers a new AgentService and triggers initial reconciliation.
func (s *Supervisor) CreateService(ctx context.Context, svc *Service) (*Service, error) {
	if svc == nil {
		return nil, ErrInvalidServiceSpec
	}
	if svc.ID == "" {
		svc.ID = "svc-" + s.idGen()[:8]
	}
	if err := svc.Validate(); err != nil {
		return nil, err
	}

	now := s.clock()
	svc.CreatedAt = now
	svc.UpdatedAt = now
	svc.Status = ServiceStatus{
		Phase:             ServicePending,
		DesiredReplicas:   svc.Spec.Replicas,
		CurrentReplicas:   0,
		ReadyReplicas:     0,
		UpdatedReplicas:   0,
		AvailableReplicas: 0,
		ActiveVersion:     svc.Spec.AgentVersion,
		RestartCount:      0,
		LastTransitionAt:  now,
		Message:           "Service created; waiting for initial reconciliation",
	}

	if err := s.store.CreateService(ctx, svc); err != nil {
		return nil, err
	}

	// Run immediate reconciliation for the newly created service
	if err := s.ReconcileService(ctx, svc); err != nil {
		slog.Error("initial reconciliation failed for service", "serviceId", svc.ID, "error", err)
	}

	return s.store.GetService(ctx, svc.TenantID, svc.ID)
}

// GetService retrieves a service by tenant and ID.
func (s *Supervisor) GetService(ctx context.Context, tenantID, serviceID string) (*Service, error) {
	return s.store.GetService(ctx, tenantID, serviceID)
}

// ListServices lists services for a tenant and namespace.
func (s *Supervisor) ListServices(ctx context.Context, tenantID, namespace string) ([]*Service, error) {
	return s.store.ListServices(ctx, tenantID, namespace)
}

// UpdateService updates service specification and reconciles.
func (s *Supervisor) UpdateService(ctx context.Context, svc *Service) (*Service, error) {
	if svc == nil {
		return nil, ErrInvalidServiceSpec
	}
	existing, err := s.store.GetService(ctx, svc.TenantID, svc.ID)
	if err != nil {
		return nil, err
	}
	if existing.Status.Phase == ServiceTerminated {
		return nil, ErrServiceTerminated
	}

	if err := svc.Validate(); err != nil {
		return nil, err
	}

	svc.Status = existing.Status
	svc.Status.DesiredReplicas = svc.Spec.Replicas
	if svc.Spec.AgentVersion != "" && svc.Spec.AgentVersion != existing.Spec.AgentVersion {
		svc.Status.PreviousVersion = existing.Spec.AgentVersion
	}
	svc.UpdatedAt = s.clock()

	if err := s.store.UpdateService(ctx, svc); err != nil {
		return nil, err
	}

	if err := s.ReconcileService(ctx, svc); err != nil {
		slog.Error("reconcile failed after service update", "serviceId", svc.ID, "error", err)
	}

	return s.store.GetService(ctx, svc.TenantID, svc.ID)
}

// ScaleService changes the desired replica count.
func (s *Supervisor) ScaleService(ctx context.Context, tenantID, serviceID string, replicas int) (*Service, error) {
	if replicas < 0 {
		return nil, fmt.Errorf("%w: replicas must be non-negative", ErrInvalidServiceSpec)
	}
	svc, err := s.store.GetService(ctx, tenantID, serviceID)
	if err != nil {
		return nil, err
	}
	if svc.Status.Phase == ServiceTerminated {
		return nil, ErrServiceTerminated
	}

	svc.Spec.Replicas = replicas
	svc.Status.DesiredReplicas = replicas
	svc.UpdatedAt = s.clock()

	if err := s.store.UpdateService(ctx, svc); err != nil {
		return nil, err
	}

	if err := s.ReconcileService(ctx, svc); err != nil {
		slog.Error("reconcile failed after service scale", "serviceId", svc.ID, "error", err)
	}

	return s.store.GetService(ctx, tenantID, serviceID)
}

// RestartService marks all active instances for termination and spawns replacements.
func (s *Supervisor) RestartService(ctx context.Context, tenantID, serviceID string) error {
	svc, err := s.store.GetService(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}
	if svc.Status.Phase == ServiceTerminated {
		return ErrServiceTerminated
	}

	instances, err := s.store.ListInstances(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	now := s.clock()
	for _, inst := range instances {
		if !inst.IsTerminal() {
			inst.Phase = InstanceStopped
			inst.TerminatedAt = &now
			inst.ExitCode = 0
			inst.ExitReason = "operator restart requested"
			_ = s.store.UpdateInstance(ctx, inst)
			_ = s.spawner.StopInstance(ctx, svc, inst)
		}
	}

	return s.ReconcileService(ctx, svc)
}

// StopService gracefully stops all instances and sets replicas to 0.
func (s *Supervisor) StopService(ctx context.Context, tenantID, serviceID string) error {
	svc, err := s.store.GetService(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}
	if svc.Status.Phase == ServiceTerminated {
		return ErrServiceTerminated
	}

	svc.Spec.Replicas = 0
	svc.Status.DesiredReplicas = 0
	svc.Status.Phase = ServiceSuspended
	svc.Status.Message = "Service stopped by operator"
	svc.UpdatedAt = s.clock()

	if err := s.store.UpdateService(ctx, svc); err != nil {
		return err
	}

	instances, err := s.store.ListInstances(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	now := s.clock()
	for _, inst := range instances {
		if !inst.IsTerminal() {
			inst.Phase = InstanceStopped
			inst.TerminatedAt = &now
			inst.ExitReason = "service stopped"
			_ = s.store.UpdateInstance(ctx, inst)
			_ = s.spawner.StopInstance(ctx, svc, inst)
		}
	}

	return s.ReconcileService(ctx, svc)
}

// DeleteService stops all running instances and marks the service terminated.
func (s *Supervisor) DeleteService(ctx context.Context, tenantID, serviceID string) error {
	svc, err := s.store.GetService(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	instances, err := s.store.ListInstances(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	now := s.clock()
	for _, inst := range instances {
		if !inst.IsTerminal() {
			inst.Phase = InstanceStopped
			inst.TerminatedAt = &now
			inst.ExitReason = "service deleted"
			_ = s.store.UpdateInstance(ctx, inst)
			_ = s.spawner.StopInstance(ctx, svc, inst)
		}
	}

	return s.store.DeleteService(ctx, tenantID, serviceID)
}

// RecordHeartbeat refreshes an instance's liveness timestamp and resets failure counts.
func (s *Supervisor) RecordHeartbeat(ctx context.Context, tenantID, serviceID, instanceID string) error {
	inst, err := s.store.GetInstance(ctx, tenantID, serviceID, instanceID)
	if err != nil {
		return err
	}
	if inst.IsTerminal() {
		return ErrInstanceTerminated
	}

	now := s.clock()
	inst.LastHeartbeat = now
	inst.ConsecutiveFailures = 0
	if inst.Phase == InstanceStarting || inst.Phase == InstanceDegraded {
		inst.Phase = InstanceRunning
	}
	inst.UpdatedAt = now

	return s.store.UpdateInstance(ctx, inst)
}

// ReportInstanceExit records a crash or clean termination of a service instance.
func (s *Supervisor) ReportInstanceExit(ctx context.Context, tenantID, serviceID, instanceID string, exitCode int, reason string) error {
	inst, err := s.store.GetInstance(ctx, tenantID, serviceID, instanceID)
	if err != nil {
		return err
	}

	now := s.clock()
	inst.TerminatedAt = &now
	inst.ExitCode = exitCode
	inst.ExitReason = reason
	if exitCode == 0 {
		inst.Phase = InstanceStopped
	} else {
		inst.Phase = InstanceFailed
	}
	inst.UpdatedAt = now

	if err := s.store.UpdateInstance(ctx, inst); err != nil {
		return err
	}

	svc, err := s.store.GetService(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}

	return s.ReconcileService(ctx, svc)
}

// DrainInstance transitions an instance to Draining and sets a deadline for graceful shutdown.
func (s *Supervisor) DrainInstance(ctx context.Context, tenantID, serviceID, instanceID string, timeout time.Duration) error {
	inst, err := s.store.GetInstance(ctx, tenantID, serviceID, instanceID)
	if err != nil {
		return err
	}
	if inst.IsTerminal() {
		return ErrInstanceTerminated
	}

	now := s.clock()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := now.Add(timeout)
	inst.Phase = InstanceDraining
	inst.DrainingAt = &now
	inst.DrainDeadline = &deadline
	inst.UpdatedAt = now

	if err := s.store.UpdateInstance(ctx, inst); err != nil {
		return err
	}

	svc, err := s.store.GetService(ctx, tenantID, serviceID)
	if err != nil {
		return err
	}
	return s.ReconcileService(ctx, svc)
}

// RolloutUpgrade updates the service's target version and triggers rolling reconciliation.
func (s *Supervisor) RolloutUpgrade(ctx context.Context, tenantID, serviceID, newVersion string) (*Service, error) {
	if strings.TrimSpace(newVersion) == "" {
		return nil, fmt.Errorf("%w: target agent version cannot be empty", ErrInvalidServiceSpec)
	}

	svc, err := s.store.GetService(ctx, tenantID, serviceID)
	if err != nil {
		return nil, err
	}
	if svc.Status.Phase == ServiceTerminated {
		return nil, ErrServiceTerminated
	}

	if svc.Spec.AgentVersion == newVersion {
		return svc, nil
	}

	svc.Status.PreviousVersion = svc.Spec.AgentVersion
	svc.Spec.AgentVersion = newVersion
	svc.UpdatedAt = s.clock()

	if err := s.store.UpdateService(ctx, svc); err != nil {
		return nil, err
	}

	if err := s.ReconcileService(ctx, svc); err != nil {
		slog.Error("reconcile failed after rollout upgrade", "serviceId", svc.ID, "error", err)
	}

	return s.store.GetService(ctx, tenantID, serviceID)
}

// RollbackService reverts the service's agent version to the previous recorded version.
func (s *Supervisor) RollbackService(ctx context.Context, tenantID, serviceID string) (*Service, error) {
	svc, err := s.store.GetService(ctx, tenantID, serviceID)
	if err != nil {
		return nil, err
	}
	if svc.Status.Phase == ServiceTerminated {
		return nil, ErrServiceTerminated
	}
	if svc.Status.PreviousVersion == "" {
		return nil, ErrNoPreviousVersion
	}

	prev := svc.Status.PreviousVersion
	curr := svc.Spec.AgentVersion
	svc.Spec.AgentVersion = prev
	svc.Status.PreviousVersion = curr
	svc.UpdatedAt = s.clock()

	if err := s.store.UpdateService(ctx, svc); err != nil {
		return nil, err
	}

	if err := s.ReconcileService(ctx, svc); err != nil {
		slog.Error("reconcile failed after service rollback", "serviceId", svc.ID, "error", err)
	}

	return s.store.GetService(ctx, tenantID, serviceID)
}

// Reconcile passes through all services and reconciles their instances.
func (s *Supervisor) Reconcile(ctx context.Context, tenantID string) error {
	services, err := s.store.ListServices(ctx, tenantID, "")
	if err != nil {
		return err
	}

	for _, svc := range services {
		if err := s.ReconcileService(ctx, svc); err != nil {
			slog.Error("reconcile failed for service", "serviceId", svc.ID, "tenantId", svc.TenantID, "error", err)
		}
	}
	return nil
}

// ReconcileService reconciles a single AgentService state against desired spec.
func (s *Supervisor) ReconcileService(ctx context.Context, svc *Service) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if svc == nil || svc.Status.Phase == ServiceTerminated {
		return nil
	}

	now := s.clock()
	instances, err := s.store.ListInstances(ctx, svc.TenantID, svc.ID)
	if err != nil {
		return err
	}

	// 1. Health inspection: check heartbeats
	for _, inst := range instances {
		if inst.Phase == InstanceRunning || inst.Phase == InstanceStarting || inst.Phase == InstanceDegraded {
			if !inst.LastHeartbeat.IsZero() && now.Sub(inst.LastHeartbeat) > svc.Spec.Health.HeartbeatTTL {
				inst.ConsecutiveFailures++
				if inst.ConsecutiveFailures >= svc.Spec.Health.UnhealthyThreshold {
					inst.Phase = InstanceFailed
					inst.ExitReason = fmt.Sprintf("missed %d consecutive heartbeats (TTL: %v)", inst.ConsecutiveFailures, svc.Spec.Health.HeartbeatTTL)
					inst.ExitCode = -1
					inst.TerminatedAt = &now
					_ = s.store.UpdateInstance(ctx, inst)
					_ = s.spawner.StopInstance(ctx, svc, inst)
				} else {
					inst.Phase = InstanceDegraded
					_ = s.store.UpdateInstance(ctx, inst)
				}
			}
		}
	}

	// 2. Draining progression: advance draining instances to stopped when deadline reached or mailbox drained
	instances, err = s.store.ListInstances(ctx, svc.TenantID, svc.ID)
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if inst.Phase == InstanceDraining {
			deadlineExpired := inst.DrainDeadline != nil && (now.After(*inst.DrainDeadline) || now.Equal(*inst.DrainDeadline))
			mailboxDrained := false
			if s.mailbox != nil {
				msgs, err := s.mailbox.Receive(ctx, inst.Address, 1)
				if err == nil && len(msgs) == 0 {
					mailboxDrained = true
				}
			}
			if deadlineExpired || (s.mailbox != nil && mailboxDrained) {
				inst.Phase = InstanceStopped
				inst.TerminatedAt = &now
				inst.ExitCode = 0
				inst.ExitReason = "drained"
				inst.UpdatedAt = now
				_ = s.store.UpdateInstance(ctx, inst)
				_ = s.spawner.StopInstance(ctx, svc, inst)
			}
		}
	}

	// 3. Crash recovery & restart policy handling
	instances, err = s.store.ListInstances(ctx, svc.TenantID, svc.ID)
	if err != nil {
		return err
	}
	for _, inst := range instances {
		if inst.Phase == InstanceFailed || (inst.Phase == InstanceStopped && inst.ExitReason != "scaled down" && inst.ExitReason != "service stopped" && inst.ExitReason != "drained" && inst.ExitReason != "service deleted") {
			shouldRestart := false
			switch svc.Spec.RestartPolicy {
			case RestartAlways:
				shouldRestart = true
			case RestartOnFailure:
				if inst.ExitCode != 0 {
					shouldRestart = true
				}
			case RestartNever:
				shouldRestart = false
			}

			if shouldRestart {
				maxRetries := svc.Spec.Backoff.MaxRetries
				if maxRetries > 0 && inst.RestartCount >= maxRetries {
					continue
				}

				if inst.NextRestartAt == nil {
					delay := svc.Spec.Backoff.CalculateDelay(inst.RestartCount)
					next := now.Add(delay)
					inst.NextRestartAt = &next
					_ = s.store.UpdateInstance(ctx, inst)
				}

				if inst.NextRestartAt != nil && (now.After(*inst.NextRestartAt) || now.Equal(*inst.NextRestartAt)) {
					// Time to restart this instance
					inst.RestartCount++
					inst.ConsecutiveFailures = 0
					inst.NextRestartAt = nil
					inst.Phase = InstanceRunning
					inst.LastHeartbeat = now
					inst.ExitCode = 0
					inst.ExitReason = ""
					inst.TerminatedAt = nil
					inst.UpdatedAt = now

					_ = s.store.UpdateInstance(ctx, inst)
					_ = s.spawner.SpawnInstance(ctx, svc, inst)
					svc.Status.RestartCount++
				}
			}
		}
	}

	// 4. Reload instances and categorize for rolling upgrade and scaling
	instances, err = s.store.ListInstances(ctx, svc.TenantID, svc.ID)
	if err != nil {
		return err
	}

	targetVersion := svc.Spec.AgentVersion
	var activeInstances []*Instance
	var recoveringInstances []*Instance
	var readyInstances []*Instance
	var updatedActiveInstances []*Instance
	var outdatedActiveInstances []*Instance

	for _, inst := range instances {
		if inst.IsActive() {
			activeInstances = append(activeInstances, inst)
			if inst.Phase == InstanceRunning {
				readyInstances = append(readyInstances, inst)
			}
			if targetVersion != "" {
				if inst.AgentVersion == targetVersion {
					updatedActiveInstances = append(updatedActiveInstances, inst)
				} else if inst.AgentVersion != "" {
					outdatedActiveInstances = append(outdatedActiveInstances, inst)
				}
			}
		} else if inst.IsRecovering() {
			recoveringInstances = append(recoveringInstances, inst)
		}
	}

	desired := svc.Spec.Replicas

	// AutoWake logic: wake up from 0 replicas when pending messages exist
	if svc.Spec.AutoWake && svc.Spec.Replicas == 0 {
		hasPending := s.checkPendingMessages(ctx, svc)
		if hasPending {
			desired = 1
		}
	}

	// 5. Rolling Update handling
	isRolling := len(outdatedActiveInstances) > 0 && targetVersion != ""
	if isRolling && desired > 0 {
		maxSurge := svc.Spec.Rollout.MaxSurge
		if maxSurge <= 0 {
			maxSurge = 1
		}
		maxUnavailable := svc.Spec.Rollout.MaxUnavailable
		if maxUnavailable <= 0 {
			maxUnavailable = 1
		}

		maxAllowedTotal := desired + maxSurge
		minAllowedHealthy := desired - maxUnavailable
		if minAllowedHealthy < 1 && desired > 0 {
			minAllowedHealthy = 1
		}

		// A. Surge step: spawn target version instances if needed and within maxSurge
		if len(updatedActiveInstances) < desired && len(activeInstances) < maxAllowedTotal {
			surgeCount := maxAllowedTotal - len(activeInstances)
			neededUpdated := desired - len(updatedActiveInstances)
			toSpawn := surgeCount
			if neededUpdated < toSpawn {
				toSpawn = neededUpdated
			}
			for i := 0; i < toSpawn; i++ {
				instID := fmt.Sprintf("%s-%s", svc.ID, s.idGen()[:6])
				instAddr := ipc.NewInstanceAddress(svc.TenantID, svc.Namespace, svc.AgentID, instID)
				newInstance := &Instance{
					ID:            instID,
					ServiceID:     svc.ID,
					TenantID:      svc.TenantID,
					Namespace:     svc.Namespace,
					AgentID:       svc.AgentID,
					AgentVersion:  targetVersion,
					RuntimeClass:  svc.Spec.RuntimeClass,
					Address:       instAddr,
					Phase:         InstanceRunning,
					LastHeartbeat: now,
					CreatedAt:     now,
					UpdatedAt:     now,
				}
				if err := s.store.CreateInstance(ctx, newInstance); err == nil {
					_ = s.spawner.SpawnInstance(ctx, svc, newInstance)
					activeInstances = append(activeInstances, newInstance)
					readyInstances = append(readyInstances, newInstance)
					updatedActiveInstances = append(updatedActiveInstances, newInstance)
				}
			}
		}

		// B. Drain step: if we meet minimum healthy requirement, drain outdated instances
		healthyCount := len(readyInstances)
		if healthyCount >= minAllowedHealthy && len(outdatedActiveInstances) > 0 {
			instToDrain := outdatedActiveInstances[0]
			instToDrain.Phase = InstanceDraining
			instToDrain.DrainingAt = &now
			drainTimeout := svc.Spec.DrainTimeout
			if drainTimeout <= 0 {
				drainTimeout = 30 * time.Second
			}
			deadline := now.Add(drainTimeout)
			instToDrain.DrainDeadline = &deadline
			instToDrain.UpdatedAt = now
			_ = s.store.UpdateInstance(ctx, instToDrain)
		}
	} else {
		// 6. Normal Replica Balancing (Scale Up / Graceful Scale Down)
		allocated := len(activeInstances) + len(recoveringInstances)
		if allocated < desired {
			needed := desired - allocated
			for i := 0; i < needed; i++ {
				instID := fmt.Sprintf("%s-%s", svc.ID, s.idGen()[:6])
				instAddr := ipc.NewInstanceAddress(svc.TenantID, svc.Namespace, svc.AgentID, instID)
				newInstance := &Instance{
					ID:            instID,
					ServiceID:     svc.ID,
					TenantID:      svc.TenantID,
					Namespace:     svc.Namespace,
					AgentID:       svc.AgentID,
					AgentVersion:  targetVersion,
					RuntimeClass:  svc.Spec.RuntimeClass,
					Address:       instAddr,
					Phase:         InstanceRunning,
					LastHeartbeat: now,
					CreatedAt:     now,
					UpdatedAt:     now,
				}
				if err := s.store.CreateInstance(ctx, newInstance); err == nil {
					_ = s.spawner.SpawnInstance(ctx, svc, newInstance)
					activeInstances = append(activeInstances, newInstance)
					readyInstances = append(readyInstances, newInstance)
				}
			}
		} else if len(activeInstances) > desired {
			excess := len(activeInstances) - desired
			for i := 0; i < excess; i++ {
				instToStop := activeInstances[len(activeInstances)-1-i]
				if svc.Spec.DrainTimeout > 0 {
					instToStop.Phase = InstanceDraining
					instToStop.DrainingAt = &now
					deadline := now.Add(svc.Spec.DrainTimeout)
					instToStop.DrainDeadline = &deadline
					instToStop.UpdatedAt = now
					_ = s.store.UpdateInstance(ctx, instToStop)
				} else {
					instToStop.Phase = InstanceStopped
					instToStop.TerminatedAt = &now
					instToStop.ExitReason = "scaled down"
					instToStop.UpdatedAt = now
					_ = s.store.UpdateInstance(ctx, instToStop)
					_ = s.spawner.StopInstance(ctx, svc, instToStop)
				}
			}
			activeInstances = activeInstances[:desired]
		}
	}

	// 7. Update overall service status
	readyCount := 0
	updatedCount := 0
	for _, inst := range activeInstances {
		if inst.Phase == InstanceRunning {
			readyCount++
			if targetVersion == "" || inst.AgentVersion == targetVersion {
				updatedCount++
			}
		}
	}

	svc.Status.DesiredReplicas = desired
	svc.Status.CurrentReplicas = len(activeInstances)
	svc.Status.ReadyReplicas = readyCount
	svc.Status.UpdatedReplicas = updatedCount
	svc.Status.AvailableReplicas = readyCount
	svc.Status.ActiveVersion = targetVersion

	if desired == 0 {
		if svc.Status.Phase != ServiceSuspended {
			svc.Status.Phase = ServiceSuspended
			svc.Status.LastTransitionAt = now
			svc.Status.Message = "Service has 0 desired replicas (idle/suspended)"
		}
	} else if isRolling {
		if svc.Status.Phase != ServiceDegraded {
			svc.Status.Phase = ServiceDegraded
			svc.Status.LastTransitionAt = now
			svc.Status.Message = fmt.Sprintf("Rolling update to %s in progress (%d/%d updated)", targetVersion, updatedCount, desired)
		}
	} else if readyCount == desired {
		if svc.Status.Phase != ServiceActive {
			svc.Status.Phase = ServiceActive
			svc.Status.LastTransitionAt = now
			svc.Status.Message = fmt.Sprintf("All %d replicas active and healthy", readyCount)
		}
	} else if readyCount > 0 {
		if svc.Status.Phase != ServiceDegraded {
			svc.Status.Phase = ServiceDegraded
			svc.Status.LastTransitionAt = now
			svc.Status.Message = fmt.Sprintf("%d of %d replicas healthy", readyCount, desired)
		}
	} else {
		// readyCount == 0 && desired > 0
		if svc.Status.Phase != ServiceDegraded && svc.Status.Phase != ServicePending {
			svc.Status.Phase = ServiceDegraded
			svc.Status.LastTransitionAt = now
			svc.Status.Message = "No healthy replicas running"
		}
	}

	svc.UpdatedAt = now
	return s.store.UpdateService(ctx, svc)
}

// checkPendingMessages inspects if the agent's mailbox has pending unread messages for AutoWake.
func (s *Supervisor) checkPendingMessages(ctx context.Context, svc *Service) bool {
	if s.mailbox == nil {
		return false
	}
	logicalAddr := svc.LogicalAddress()
	msgs, err := s.mailbox.Receive(ctx, logicalAddr, 1)
	if err == nil && len(msgs) > 0 {
		return true
	}
	return false
}
