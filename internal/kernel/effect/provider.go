package effect

import (
	"context"
	"errors"
	"fmt"
)

// Provider defines the execution interface for an external effect provider.
type Provider interface {
	Name() string
	Execute(ctx context.Context, req *EffectRequest) ([]byte, error)
}

// AmbiguousError indicates that the outcome of an external operation cannot be determined.
// For instance, a network connection timed out or dropped after sending the HTTP payload,
// but before the server returned an acknowledgement.
type AmbiguousError struct {
	Err error
}

func (e *AmbiguousError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("ambiguous provider execution: %v", e.Err)
	}
	return "ambiguous provider execution"
}

func (e *AmbiguousError) Unwrap() error {
	return e.Err
}

// MarkAmbiguous wraps an error into an AmbiguousError.
func MarkAmbiguous(err error) error {
	if err == nil {
		return nil
	}
	return &AmbiguousError{Err: err}
}

// IsAmbiguous tests whether an error represents an ambiguous execution outcome.
func IsAmbiguous(err error) bool {
	if err == nil {
		return false
	}
	var amb *AmbiguousError
	if errors.As(err, &amb) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
}
