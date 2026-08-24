//go:build race

package e2e_test

// The P95 reconcile gate measures the orchestrator loop, not the race
// detector. Instrumented builds inflate every store roundtrip 2-5x on
// shared runners, so the instrumented threshold doubles the calibrated
// non-race gate (see scale_gate_norace_test.go).
const reconcileP95GateMs = 1000
