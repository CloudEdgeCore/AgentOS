package store

import "context"

type RuntimePoolState struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	ResourceVersion int64  `json:"resourceVersion"`
}

type UpdateRuntimePoolStatusInput struct {
	TenantID        string
	PoolID          string
	Status          string
	ExpectedVersion int64
}

type RuntimePoolOperatorStore interface {
	UpdateRuntimePoolStatus(context.Context, UpdateRuntimePoolStatusInput) (RuntimePoolState, error)
}
