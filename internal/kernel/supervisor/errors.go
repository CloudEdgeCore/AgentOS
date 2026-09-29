package supervisor

import "errors"

var (
	// ErrServiceNotFound is returned when an agent service does not exist.
	ErrServiceNotFound = errors.New("agent service not found")

	// ErrServiceAlreadyExists is returned when attempting to create a service with an existing ID or name.
	ErrServiceAlreadyExists = errors.New("agent service already exists")

	// ErrInstanceNotFound is returned when a service instance is not found.
	ErrInstanceNotFound = errors.New("service instance not found")

	// ErrInstanceAlreadyExists is returned when an instance ID is already registered.
	ErrInstanceAlreadyExists = errors.New("service instance already exists")

	// ErrInvalidServiceSpec is returned when service specification validation fails.
	ErrInvalidServiceSpec = errors.New("invalid agent service specification")

	// ErrServiceTerminated is returned when operating on a terminated service.
	ErrServiceTerminated = errors.New("agent service is terminated")

	// ErrInstanceTerminated is returned when attempting to heartbeat or operate on a terminated instance.
	ErrInstanceTerminated = errors.New("service instance is terminated")

	// ErrNoHealthyInstance is returned when no healthy instance is available to route IPC traffic.
	ErrNoHealthyInstance = errors.New("no healthy service instance available")

	// ErrMaxRestartsExceeded is returned when an instance has reached the maximum allowed restart attempts.
	ErrMaxRestartsExceeded = errors.New("maximum restart retries exceeded")

	// ErrRolloutInProgress is returned when another rollout is already underway.
	ErrRolloutInProgress = errors.New("rollout upgrade already in progress")

	// ErrRolloutFailed is returned when an agent rollout fails health or stability checks.
	ErrRolloutFailed = errors.New("rollout upgrade failed")

	// ErrNoPreviousVersion is returned when attempting to rollback a service that has no prior version recorded.
	ErrNoPreviousVersion = errors.New("no previous service version recorded for rollback")

	// ErrInstanceDraining is returned when routing to an instance that is actively draining.
	ErrInstanceDraining = errors.New("service instance is currently draining")
)
