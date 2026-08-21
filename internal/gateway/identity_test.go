package gateway

import (
	"context"
	"testing"
)

func TestTenantAuthorizationModesFailClosed(t *testing.T) {
	if err := authorizeTenant(context.Background(), "tenant-a", "tenant-a"); err != nil {
		t.Fatalf("fixed development tenant rejected: %v", err)
	}
	if err := authorizeTenant(context.Background(), "tenant-a", "tenant-b"); err == nil {
		t.Fatal("cross-tenant development claim accepted")
	}
	if err := authorizeTenant(context.Background(), peerBoundTenant, "tenant-a"); err == nil {
		t.Fatal("production tenant accepted without a verified peer SVID")
	}
}
