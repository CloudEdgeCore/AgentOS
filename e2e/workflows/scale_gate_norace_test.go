//go:build !race

package e2e

// Calibrated orchestrator reconcile gate for uninstrumented builds
// (Phase 3 acceptance: P95 < 500ms).
const reconcileP95GateMs = 500
