// Package auth defines the authenticated identity carried by control-plane requests.
package auth

import (
	"context"
	"net/http"
	"strings"
)

type Principal struct {
	Subject  string
	TenantID string
}

type contextKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(contextKey{}).(Principal)
	return principal, ok && strings.TrimSpace(principal.Subject) != "" && strings.TrimSpace(principal.TenantID) != ""
}

// StaticMiddleware is intentionally suitable only for a single-tenant local
// development server. Production deployments must replace it with verified
// workload or user identity middleware.
func StaticMiddleware(principal Principal, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(writer, request.WithContext(WithPrincipal(request.Context(), principal)))
	})
}
