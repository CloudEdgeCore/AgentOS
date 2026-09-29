package effect

import "errors"

var (
	// ErrEffectNotFound indicates that the requested effect record does not exist.
	ErrEffectNotFound = errors.New("effect record not found")

	// ErrEffectFenced indicates that the attempt is stale or superseded and cannot execute effects.
	ErrEffectFenced = errors.New("effect rejected by fencing: attempt is stale or superseded")

	// ErrEffectUnknown indicates that the effect outcome is ambiguous and cannot be confirmed.
	// Automatic replay of an effect in UNKNOWN state is strictly prohibited.
	ErrEffectUnknown = errors.New("effect is in UNKNOWN state: outcome is ambiguous, automatic replay is strictly prohibited")

	// ErrEffectExecuting indicates that the effect is currently executing by another attempt or worker.
	ErrEffectExecuting = errors.New("effect is currently executing by another active attempt")

	// ErrPayloadMismatch indicates an idempotency key collision with a different payload hash.
	ErrPayloadMismatch = errors.New("idempotency key conflict: payload hash does not match previous request")

	// ErrProviderNotFound indicates that the requested external provider is not registered.
	ErrProviderNotFound = errors.New("effect provider not registered")

	// ErrProviderFailed indicates that the external provider execution failed with a definitive error.
	ErrProviderFailed = errors.New("effect provider execution failed")

	// ErrEffectExpired indicates that the effect request deadline has passed.
	ErrEffectExpired = errors.New("effect request deadline expired")

	// ErrInvalidRequest indicates that the effect request parameters failed validation.
	ErrInvalidRequest = errors.New("invalid effect request")

	// ErrEffectAlreadyCommitted indicates that the effect has already completed successfully.
	ErrEffectAlreadyCommitted = errors.New("effect has already been committed")

	// ErrInvalidStatusTransition indicates an invalid state transition on an effect record.
	ErrInvalidStatusTransition = errors.New("invalid effect status transition")
)
