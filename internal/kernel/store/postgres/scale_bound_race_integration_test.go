//go:build integration && race

package postgres_test

import "time"

// The 10k dynamic-spawn leg is throughput evidence, not a latency gate, but
// instrumented builds inflate every store roundtrip and shared CI runners add
// more on top (the 2m window failed nightly at 2m38s under -race). Give the
// race build the same 10m headroom as the orchestration leg in
// internal/kernel/workflow so only a real stall can time the leg out.
const v13SpawnScaleBound = 10 * time.Minute
