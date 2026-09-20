package config

import (
	"fmt"
)

// RollbackTo restores the registry state to the given commit ID.
// It loads the closest snapshot and replays WAL up to the target commit.
func (r *Registry) RollbackTo(commitID string) error {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	// 1. Find the target commit and its associated snapshot
	// For simplicity in this P0 implementation, we:
	// a. Try to find if commitID is in the active WAL.

	// 2. Load the base snapshot (config.json)
	if err := r.loadWithFallbackLocked(); err != nil {
		return err
	}

	// 3. Replay WAL UP TO commitID
	if r.wal != nil {
		if err := r.wal.ReplayUntil(r, commitID); err != nil {
			return fmt.Errorf("rollback: failed to replay to %s: %w", commitID, err)
		}
	} else {
		return fmt.Errorf("rollback: WAL is not enabled, cannot rollback to commit ID")
	}

	// 4. Force a new snapshot and clear WAL
	return r.PersistForceSnapshot()
}
