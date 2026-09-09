//go:build !race

package workflow

import "time"

// Uninstrumented builds finish the 10k dynamic-task leg in ~4s locally, so a
// 60s window still catches a genuine orchestration stall (see
// scale_deadline_race_test.go for the instrumented threshold).
const v13ScaleLegWait = 60 * time.Second
