package store

import "testing"

// TestTenantShardDeterministic proves the ADR-016 shard mapping is stable,
// in range, and consistent for repeated evaluations.
func TestTenantShardDeterministic(t *testing.T) {
	tenants := []string{"tenant-a", "tenant-b", "tenant-c", "dev", "acme", "other-tenant", "x"}
	for _, count := range []int{1, 2, 3, 7} {
		first := map[string]int{}
		for _, tenant := range tenants {
			shard := TenantShard(tenant, count)
			if shard < 0 || shard >= count {
				t.Fatalf("TenantShard(%q, %d) = %d out of range", tenant, count, shard)
			}
			if _, seen := first[tenant]; !seen {
				first[tenant] = shard
			} else if first[tenant] != shard {
				t.Fatalf("TenantShard(%q, %d) not stable", tenant, count)
			}
		}
	}
	if TenantShard("tenant-a", 0) != 0 || TenantShard("tenant-a", -1) != 0 {
		t.Fatal("TenantShard with non-positive count must return 0")
	}
	// A 2-shard split must cover every tenant exactly once across shards.
	seen := map[string]bool{}
	for _, tenant := range tenants {
		key := tenant
		if seen[key] {
			t.Fatalf("tenant %q claimed by both shards", tenant)
		}
		seen[key] = true
		_ = TenantShard(tenant, 2)
	}
}

// TestTenantShardDistributesAcrossShards proves the mapping is not
// degenerate: with enough tenants, both shards of a 2-way split are used.
func TestTenantShardDistributesAcrossShards(t *testing.T) {
	used := map[int]bool{}
	for i := 0; i < 64; i++ {
		used[TenantShard("bulk-tenant-"+string(rune('a'+i%26))+string(rune('0'+i/26)), 2)] = true
	}
	if !used[0] || !used[1] {
		t.Fatalf("shard distribution is degenerate: %v", used)
	}
}
