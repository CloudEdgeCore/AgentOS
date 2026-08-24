//go:build !race

package workflows

// Calibrated orchestrator reconcile gate for uninstrumented builds
// (Phase 3 acceptance: P95 < 500ms).
const reconcileP95GateMs = 500
