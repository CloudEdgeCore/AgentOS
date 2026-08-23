package store

import (
	"context"
	"errors"
)

// ErrPoolOperatorDenied reports a pool status change attempted by a
// principal without an operator grant. Tenant usage grants never imply
// operator authority on shared pools.
var ErrPoolOperatorDenied = errors.New("runtime pool operator grant is required")

type RuntimePoolState struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	ResourceVersion int64  `json:"resourceVersion"`
}

type UpdateRuntimePoolStatusInput struct {
	TenantID string
	PoolID   string
	Status   string
	// OperatorSubject is the authenticated principal requesting the status
	// change. It must hold a runtime_pool_operator_grants row for the pool;
	// the tenant usage grant alone is never sufficient.
	OperatorSubject string
	ExpectedVersion int64
}

type RuntimePoolOperatorStore interface {
	UpdateRuntimePoolStatus(context.Context, UpdateRuntimePoolStatusInput) (RuntimePoolState, error)
}
