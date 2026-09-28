package namespace

import "errors"

var (
	// ErrNamespaceNotFound indicates that the requested namespace does not exist.
	ErrNamespaceNotFound = errors.New("namespace not found")

	// ErrNamespaceAlreadyExists indicates that a namespace with the same name already exists in the tenant.
	ErrNamespaceAlreadyExists = errors.New("namespace already exists")

	// ErrInvalidNamespaceName indicates that the namespace name violates RFC 1123 DNS-1123 label standards.
	ErrInvalidNamespaceName = errors.New("invalid namespace name")

	// ErrNamespaceTerminating indicates that the namespace is in Terminating phase and cannot accept new resources.
	ErrNamespaceTerminating = errors.New("namespace is terminating")

	// ErrDefaultNamespaceProtected indicates an attempt to delete or decommission the immutable 'default' namespace.
	ErrDefaultNamespaceProtected = errors.New("the default namespace is protected and cannot be deleted")

	// ErrQuotaExceeded indicates a resource request exceeds the limits defined in the namespace's ResourceQuota.
	ErrQuotaExceeded = errors.New("resource quota exceeded")

	// ErrNamespaceNotEmpty indicates that a namespace cannot be purged because it still contains active resources.
	ErrNamespaceNotEmpty = errors.New("namespace contains active resources and cannot be deleted")

	// ErrCrossNamespaceForbidden indicates that cross-namespace communication or access is disallowed by policy.
	ErrCrossNamespaceForbidden = errors.New("cross-namespace access is forbidden by policy")
)
