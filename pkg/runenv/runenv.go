package runenv

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// ResolveDataRoot resolves the data root directory using a unified chain:
//  1. VORTEX_ROOT env var (explicit pin, highest priority)
//  2. Binary-relative walk-up: from exe dir, walk up ≤5 layers looking for
//     config.json or config_<GOOS>.json (self-contained deployment)
//  3. User data dir: $XDG_DATA_HOME/orchestrator or ~/.local/share/orchestrator
//     (deterministic fallback when os.Executable() fails, e.g. sandboxed)
//  4. fail-closed: return error (never silently fall back to cwd)
//
// Spec: docs/plan/DATAROOT_PATH_UNIFICATION.md
func ResolveDataRoot() (string, error) {
	if r := os.Getenv("VORTEX_ROOT"); r != "" {
		return r, nil
	}
	exe, err := os.Executable()
	if err == nil {
		start := filepath.Dir(exe)
		if root, ok := walkUpFrom(start); ok {
			return root, nil
		}
	}
	return userDataDir()
}

// walkUpFrom walks up from start directory, checking at most 5 layers for
// config.json or config_<GOOS>.json. Returns the directory containing the
// config file and true if found, or "" and false if not.
func walkUpFrom(start string) (string, bool) {
	r := start
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(r, "config.json")); err == nil {
			return r, true
		}
		if _, err := os.Stat(filepath.Join(r, "config_"+runtime.GOOS+".json")); err == nil {
			return r, true
		}
		parent := filepath.Dir(r)
		if parent == r {
			break
		}
		r = parent
	}
	return "", false
}

// userDataDir returns the OS-appropriate user data directory for orchestrator:
// $XDG_DATA_HOME/orchestrator on Linux, or ~/.local/share/orchestrator as
// fallback. Returns an error if neither XDG_DATA_HOME nor HOME is set.
func userDataDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "orchestrator"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("无法解析数据根：VORTEX_ROOT 未设、二进制目录无 config.json、且无法确定用户目录；请设置 VORTEX_ROOT 环境变量")
	}
	return filepath.Join(home, ".local", "share", "orchestrator"), nil
}

// Centralized log-path decisions for all entry points
// (main.go / main_web.go / main_desktop.go / main_dev.go).
//
// Rationale (docs/audit/deletion-review-log.md, 2026-10-24): each log artifact's
// filename was previously repeated inline at every wiring site
// (filepath.Join(logDir, "audit_chain.json"), …), so the "generation point"
// of each log was scattered. Keeping the literals here gives a single source
// of truth. These are one-liners on purpose so call sites stay line-neutral —
// the entry-point functions are enforced by the size gate (func ≤ 150 / file
// ≤ 1000, frozen baseline).
func LogsDir(root string) string          { return filepath.Join(root, "logs") }
func AuditChainPath(logDir string) string { return filepath.Join(logDir, "audit_chain.json") }
func CostDBPath(logDir string) string     { return filepath.Join(logDir, "cost_records.db") }
func TrajectoryPath(logDir string) string { return filepath.Join(logDir, "global_trajectory.jsonl") }
