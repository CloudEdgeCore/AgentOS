package namespace

import "context"

// Store defines persistence and retrieval operations for namespaces and their resource usage.
type Store interface {
	// CreateNamespace creates a new namespace in the given tenant.
	// Returns ErrNamespaceAlreadyExists if a namespace with the same name exists.
	// Returns ErrInvalidNamespaceName if name does not satisfy RFC 1123.
	CreateNamespace(ctx context.Context, ns *Namespace) error

	// GetNamespace retrieves a namespace by tenant and name.
	// If the name is "default" and has not been explicitly created yet, the store
	// should transparently initialize and return the default namespace.
	// Returns ErrNamespaceNotFound if not found.
	GetNamespace(ctx context.Context, tenantID, name string) (*Namespace, error)

	// UpdateNamespace updates the metadata, labels, and quota of an existing namespace.
	// Returns ErrNamespaceNotFound if it does not exist.
	UpdateNamespace(ctx context.Context, ns *Namespace) error

	// DeleteNamespace marks or deletes a namespace.
	// Returns ErrDefaultNamespaceProtected if name == "default".
	// Returns ErrNamespaceNotFound if it does not exist.
	// Returns ErrNamespaceNotEmpty if the namespace currently holds active workloads or services.
	DeleteNamespace(ctx context.Context, tenantID, name string) error

	// ListNamespaces lists all namespaces within a tenant.
	ListNamespaces(ctx context.Context, tenantID string) ([]*Namespace, error)

	// GetUsage returns the current active and settled resource usage in the namespace.
	GetUsage(ctx context.Context, tenantID, name string) (*ResourceUsage, error)

	// RecordUsageDelta applies incremental changes to resource usage counters.
	RecordUsageDelta(ctx context.Context, tenantID, name string, delta ResourceUsageDelta) error
}
