//go:build integration && !race

package postgres_test

import "time"

// Uninstrumented builds commit the 10k dynamic-spawn leg in well under a
// minute locally, so 2m still catches a genuine store stall (see
// scale_bound_race_integration_test.go for the instrumented threshold).
const v13SpawnScaleBound = 2 * time.Minute
