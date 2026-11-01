package runenv

import (
	"os"
	"path/filepath"
)

// ResolveDataRoot returns the VORTEX_ROOT env var if set, otherwise calls
// fallback. This lets automated deployments pin the data root (outputs/, db,
// logs/, .env) explicitly instead of relying on exe-dir/cwd heuristics.
//
// Spec: docs/plan/DELEGATION_HANDOVER_REDESIGN.md §13.6.
// Default (env unset) is identical to prior behavior — backward compatible.
func ResolveDataRoot(fallback func() string) string {
	if r := os.Getenv("VORTEX_ROOT"); r != "" {
		return r
	}
	return fallback()
}

// Centralized log-path decisions for all entry points
// (main.go / main_web.go / main_desktop.go / main_dev.go).
//
// Rationale (docs/deletion-review-log.md, 2026-10-24): each log artifact's
// filename was previously repeated inline at every wiring site
// (filepath.Join(logDir, "audit_chain.json"), …), so the "generation point"
// of each log was scattered. Keeping the literals here gives a single source
// of truth. These are one-liners on purpose so call sites stay line-neutral —
// the entry-point functions are enforced by the size gate (func ≤ 150 / file
// ≤ 1000, frozen baseline).
func LogsDir(root string) string        { return filepath.Join(root, "logs") }
func AuditChainPath(logDir string) string { return filepath.Join(logDir, "audit_chain.json") }
func CostDBPath(logDir string) string     { return filepath.Join(logDir, "cost_records.db") }
func TrajectoryPath(logDir string) string { return filepath.Join(logDir, "global_trajectory.jsonl") }
