package main

import "path/filepath"

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
func logsDir(root string) string { return filepath.Join(root, "logs") }

func auditChainPath(logDir string) string { return filepath.Join(logDir, "audit_chain.json") }

func costDBPath(logDir string) string { return filepath.Join(logDir, "cost_records.db") }

func trajectoryPath(logDir string) string { return filepath.Join(logDir, "global_trajectory.jsonl") }
