package namespace

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestValidateNamespaceName(t *testing.T) {
	valid := []string{
		"default",
		"production",
		"dev-1",
		"ns-abc-123",
		"a",
		"1",
		"system-monitoring-infra",
	}
	for _, name := range valid {
		if err := ValidateNamespaceName(name); err != nil {
			t.Errorf("expected valid name %q, got error: %v", name, err)
		}
	}

	invalid := []string{
		"",
		"-start-with-hyphen",
		"end-with-hyphen-",
		"UPPERCASE",
		"has spaces",
		"has_underscore",
		"has.dot",
		"has/slash",
		"a-very-long-name-that-exceeds-sixty-three-characters-which-is-strictly-prohibited-by-rfc1123-spec",
	}
	for _, name := range invalid {
		if err := ValidateNamespaceName(name); err == nil {
			t.Errorf("expected invalid name %q to return error, got nil", name)
		}
	}
}

func TestMemoryStoreCRUD(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	tenant := "tenant-alpha"

	// 1. Get default namespace should succeed automatically
	def, err := store.GetNamespace(ctx, tenant, DefaultNamespace)
	if err != nil {
		t.Fatalf("failed to get default namespace: %v", err)
	}
	if def.Name != DefaultNamespace || def.Phase != NamespacePhaseActive {
		t.Fatalf("unexpected default namespace: %+v", def)
	}

	// 2. Cannot delete default namespace
	err = store.DeleteNamespace(ctx, tenant, DefaultNamespace)
	if !errors.Is(err, ErrDefaultNamespaceProtected) {
		t.Fatalf("expected ErrDefaultNamespaceProtected, got: %v", err)
	}

	// 3. Create a custom namespace
	custom := &Namespace{
		TenantID:    tenant,
		Name:        "research-team",
		DisplayName: "Research & Development",
		Description: "Namespace for research experiments",
		Labels: map[string]string{
			"team": "research",
		},
		Quota: ResourceQuota{
			MaxServices:            5,
			MaxTasks:               20,
			AllowCrossNamespaceIPC: true,
		},
	}
	err = store.CreateNamespace(ctx, custom)
	if err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// 4. Duplicate creation should fail
	err = store.CreateNamespace(ctx, custom)
	if !errors.Is(err, ErrNamespaceAlreadyExists) {
		t.Fatalf("expected ErrNamespaceAlreadyExists, got: %v", err)
	}

	// 5. Get created namespace
	got, err := store.GetNamespace(ctx, tenant, "research-team")
	if err != nil {
		t.Fatalf("failed to get namespace: %v", err)
	}
	if got.DisplayName != "Research & Development" || got.Quota.MaxServices != 5 {
		t.Fatalf("unexpected namespace data: %+v", got)
	}

	// 6. List namespaces
	list, err := store.ListNamespaces(ctx, tenant)
	if err != nil {
		t.Fatalf("failed to list namespaces: %v", err)
	}
	if len(list) != 2 { // default and research-team
		t.Fatalf("expected 2 namespaces, got %d", len(list))
	}

	// 7. Update namespace
	got.DisplayName = "Research Advanced"
	got.Quota.MaxServices = 10
	err = store.UpdateNamespace(ctx, got)
	if err != nil {
		t.Fatalf("failed to update namespace: %v", err)
	}
	updated, _ := store.GetNamespace(ctx, tenant, "research-team")
	if updated.DisplayName != "Research Advanced" || updated.Quota.MaxServices != 10 {
		t.Fatalf("unexpected updated data: %+v", updated)
	}

	// 8. Cannot delete namespace with active resources
	_ = store.RecordUsageDelta(ctx, tenant, "research-team", ResourceUsageDelta{ActiveServicesDelta: 1})
	err = store.DeleteNamespace(ctx, tenant, "research-team")
	if !errors.Is(err, ErrNamespaceNotEmpty) {
		t.Fatalf("expected ErrNamespaceNotEmpty, got: %v", err)
	}

	// Decrement active resource
	_ = store.RecordUsageDelta(ctx, tenant, "research-team", ResourceUsageDelta{ActiveServicesDelta: -1})

	// 9. Successfully delete empty namespace
	err = store.DeleteNamespace(ctx, tenant, "research-team")
	if err != nil {
		t.Fatalf("failed to delete empty namespace: %v", err)
	}

	// 10. Verify it is gone
	_, err = store.GetNamespace(ctx, tenant, "research-team")
	if !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("expected ErrNamespaceNotFound, got: %v", err)
	}
}

func TestMemoryStoreUsageTracking(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store := NewMemoryStoreWithClock(func() time.Time { return now })

	tenant := "tenant-beta"
	nsName := "analytics"

	err := store.CreateNamespace(ctx, &Namespace{
		TenantID: tenant,
		Name:     nsName,
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	delta := ResourceUsageDelta{
		ActiveServicesDelta:       2,
		ActiveTasksDelta:          5,
		ConsumedTokensDelta:       1500,
		ConsumedCostMicroUSDDelta: 5000,
		MailboxMessagesDelta:      10,
	}
	err = store.RecordUsageDelta(ctx, tenant, nsName, delta)
	if err != nil {
		t.Fatalf("record delta failed: %v", err)
	}

	usage, err := store.GetUsage(ctx, tenant, nsName)
	if err != nil {
		t.Fatalf("get usage failed: %v", err)
	}
	if usage.ActiveServices != 2 || usage.ActiveTasks != 5 || usage.ConsumedTokens != 1500 || usage.ConsumedCostMicroUSD != 5000 || usage.MailboxMessages != 10 {
		t.Fatalf("unexpected usage counters: %+v", usage)
	}
}
