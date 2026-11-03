package core

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// AssetManager handles the side-loading of large task outputs.
type AssetManager struct {
	StorePath string
	Threshold int // bytes
	Registry  *config.Registry
}

func NewAssetManager(storePath string, threshold int, reg *config.Registry) *AssetManager {
	if threshold == 0 {
		threshold = 51200 // 50KB default
	}
	_ = os.MkdirAll(storePath, 0755)
	return &AssetManager{
		StorePath: storePath,
		Threshold: threshold,
		Registry:  reg,
	}
}

// validateID rejects IDs that contain path separators or traversal sequences.
// This prevents path traversal attacks via crafted taskID/stepID values that
// could escape the asset store directory (audit M3).
func validateID(id string) error {
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return fmt.Errorf("invalid ID: path separator or traversal detected: %q", id)
	}
	return nil
}

// Handle checks data size and side-loads it if it exceeds the threshold.
// Returns an OutputFile if side-loaded, or nil if data is small enough to stay inline.
func (am *AssetManager) Handle(taskID string, stepID string, data []byte, format string, kind string) (*schemas.OutputFile, error) {
	if len(data) <= am.Threshold {
		return nil, nil
	}

	// Sanitize taskID and stepID to prevent path traversal (audit M3).
	if err := validateID(taskID); err != nil {
		return nil, fmt.Errorf("asset_manager: invalid task ID: %w", err)
	}
	if err := validateID(stepID); err != nil {
		return nil, fmt.Errorf("asset_manager: invalid step ID: %w", err)
	}

	taskDir, err := safeJoin(am.StorePath, taskID)
	if err != nil {
		return nil, fmt.Errorf("asset_manager: invalid task ID path: %w", err)
	}
	if kind != "" {
		taskDir = filepath.Join(taskDir, kind)
	}
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		return nil, fmt.Errorf("create task asset dir: %w", err)
	}

	fileName := fmt.Sprintf("%s_%d.%s", stepID, time.Now().Unix(), format)
	if format == "" {
		fileName = fmt.Sprintf("%s_%d.bin", stepID, time.Now().Unix())
	}
	filePath := filepath.Join(taskDir, fileName)

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return nil, fmt.Errorf("write asset file: %w", err)
	}

	return &schemas.OutputFile{
		StepID:    stepID,
		Path:      filePath,
		Format:    format,
		SizeBytes: len(data),
		CreatedAt: time.Now(),
		IsPrimary: true,
	}, nil
}

// sweepStats records the outcome of a single sweep pass (§4.1).
type sweepStats struct {
	scanned        int
	stagingCleaned int
	candidates     int // deletion candidates (task_dirs mode only)
	skipped        int // skipped: nil registry, query error, or non-terminal
}

// Sentry is a background routine that cleans up staging data and identifies
// deletion candidates. It runs until ctx is cancelled.
//
// D-GC-1 fix (§4.1): replaces the old ts.Get(taskID, "") existence check
// (which always failed with empty stepID → deleted every task dir) with
// TaskRegistry.LoadTask. Per §3.3, the background sweep NEVER auto-deletes
// task directories — deletion is via explicit orchestrator_cleanup_workspace.
func (am *AssetManager) Sentry(ctx context.Context, tr store.TaskRegistry) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		stats := am.sweepOnce(ctx, time.Now(), tr)
		log.Printf("asset_gc: scanned=%d staging_cleaned=%d candidates=%d skipped=%d",
			stats.scanned, stats.stagingCleaned, stats.candidates, stats.skipped)
	}
}

// sweepOnce performs a single sweep pass. Extracted from Sentry for testability.
//
// Mode behavior:
//   - off: no file-side cleanup at all.
//   - staging_only (default): clean .staging via CleanupStaging. Never delete dirs.
//   - task_dirs: same as staging_only + identify terminal dirs older than
//     AssetTTLDays as deletion candidates (reported only, NOT auto-deleted).
//
// Safety invariants (§3.1–§3.3):
//   - nil TaskRegistry → skip, never delete (persistence unavailable).
//   - query error → skip, never delete (DB busy/IO ≠ not-found).
//   - non-terminal status → skip, never delete.
func (am *AssetManager) sweepOnce(ctx context.Context, now time.Time, tr store.TaskRegistry) sweepStats {
	stats := sweepStats{}

	mode := am.assetGCMode()
	if mode == "off" {
		return stats
	}

	entries, err := os.ReadDir(am.StorePath)
	if err != nil {
		return stats
	}

	ttl := am.stagingTTL()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		stats.scanned++

		taskID := entry.Name()
		taskPath := filepath.Join(am.StorePath, taskID)

		// Always clean .staging in staging_only and task_dirs modes.
		sw := NewStagedWorkspace(taskPath)
		if err := sw.CleanupStaging(ttl, false); err == nil {
			stats.stagingCleaned++
		}

		// Only task_dirs mode identifies deletion candidates.
		if mode != "task_dirs" {
			continue
		}

		if tr == nil {
			stats.skipped++
			continue
		}

		row, _, err := tr.LoadTask(ctx, taskID)
		if err != nil {
			// Query failed (DB busy, IO, etc.) — do NOT assume "not found".
			stats.skipped++
			continue
		}
		if row == nil {
			// Deterministic not-found → orphan candidate.
			stats.candidates++
			continue
		}
		if !isTerminalTaskStatus(row.Status) {
			// Non-terminal: never a deletion candidate (§3.1).
			continue
		}

		// Terminal: check age against AssetTTLDays.
		ttlDays := am.assetTTLDays()
		if ttlDays > 0 && taskAgeDays(row.UpdatedAt, now) > ttlDays {
			stats.candidates++
		}
	}

	return stats
}

// assetGCMode returns the configured asset GC mode, defaulting to "staging_only".
func (am *AssetManager) assetGCMode() string {
	if am.Registry == nil {
		return "staging_only"
	}
	mode := am.Registry.System.Retention.AssetGC
	switch mode {
	case "off", "staging_only", "task_dirs":
		return mode
	default:
		return "staging_only" // invalid → conservative default
	}
}

// stagingTTL returns the .staging cleanup TTL from config (default 24h).
func (am *AssetManager) stagingTTL() time.Duration {
	hours := 24
	if am.Registry != nil && am.Registry.System.StagingRetentionHours > 0 {
		hours = am.Registry.System.StagingRetentionHours
	}
	return time.Duration(hours) * time.Hour
}

// assetTTLDays returns the configured TTL days, defaulting to 365.
func (am *AssetManager) assetTTLDays() int {
	if am.Registry != nil && am.Registry.System.Retention.AssetTTLDays > 0 {
		return am.Registry.System.Retention.AssetTTLDays
	}
	return 365
}

// isTerminalTaskStatus checks if a string status is terminal (completed/failed/cancelled).
// Mirrors isTerminalStatus in scheduler.go but works on raw strings from TaskRow.
func isTerminalTaskStatus(status string) bool {
	switch status {
	case "completed", "completed_with_skips", "failed", "cancelled":
		return true
	}
	return false
}

// taskAgeDays parses a SQLite CURRENT_TIMESTAMP string and returns age in days.
// Returns 0 on parse failure (conservative: treat as not-old-enough to delete).
func taskAgeDays(updatedAt string, now time.Time) int {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", updatedAt, time.Local)
	if err != nil {
		return 0
	}
	return int(now.Sub(t).Hours() / 24)
}
