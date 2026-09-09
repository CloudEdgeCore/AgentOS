//go:build race

package workflow

import "time"

// The 10k dynamic-task leg is throughput evidence, not a latency gate, but
// instrumented builds inflate every fake-store roundtrip 2-5x and shared CI
// runners add another 2-4x on top (the 30s non-race window failed nightly on
// slow runners while fast ones passed). Give the race build enough headroom
// that only a real stall, not runner speed, can time the leg out.
const v13ScaleLegWait = 10 * time.Minute
