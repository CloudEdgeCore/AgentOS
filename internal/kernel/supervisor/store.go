package supervisor

import "context"

// Store defines persistence operations for Agent Services and their Instances.
type Store interface {
	// Service operations
	CreateService(ctx context.Context, svc *Service) error
	GetService(ctx context.Context, tenantID, serviceID string) (*Service, error)
	GetServiceByName(ctx context.Context, tenantID, namespace, name string) (*Service, error)
	UpdateService(ctx context.Context, svc *Service) error
	DeleteService(ctx context.Context, tenantID, serviceID string) error
	ListServices(ctx context.Context, tenantID, namespace string) ([]*Service, error)

	// Instance operations
	CreateInstance(ctx context.Context, inst *Instance) error
	GetInstance(ctx context.Context, tenantID, serviceID, instanceID string) (*Instance, error)
	UpdateInstance(ctx context.Context, inst *Instance) error
	DeleteInstance(ctx context.Context, tenantID, serviceID, instanceID string) error
	ListInstances(ctx context.Context, tenantID, serviceID string) ([]*Instance, error)
	ListAllInstances(ctx context.Context, tenantID string) ([]*Instance, error)
}
