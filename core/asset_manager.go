package core

import (
	"context"
	"fmt"
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

// Sentry is a background routine that cleans up orphaned assets and staging data.
// It runs until ctx is cancelled.
func (am *AssetManager) Sentry(ctx context.Context, ts store.ITaskStore) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		entries, err := os.ReadDir(am.StorePath)
		if err != nil {
			continue
		}

		// Retention policy
		retentionHours := 24
		if am.Registry != nil && am.Registry.System.StagingRetentionHours > 0 {
			retentionHours = am.Registry.System.StagingRetentionHours
		}
		ttl := time.Duration(retentionHours) * time.Hour

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			taskID := entry.Name()
			taskPath := filepath.Join(am.StorePath, taskID)

			// Check if task exists in store
			task, err := ts.Get(ctx, taskID, "")
			if err != nil || task == nil {
				// Task not found, delete entire directory
				_ = os.RemoveAll(taskPath)
				continue
			}

			// Task exists: cleanup StagedWorkspace data if expired
			sw := NewStagedWorkspace(taskPath)
			// For finished tasks, we can be more aggressive.
			// For now, use the same TTL for all .staging data.
			// keepJournal=true for failed tasks could be an optimization.
			_ = sw.CleanupStaging(ttl, false)
		}
	}
}
