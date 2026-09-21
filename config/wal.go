package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Commit represents a batch of operations appended to the WAL.
type Commit struct {
	ID     string    `json:"id"`
	TS     time.Time `json:"ts"`
	Actor  string    `json:"actor"`
	TaskID string    `json:"task_id,omitempty"`
	Ops    []PatchOp `json:"ops"`
}

// WAL manages the write-ahead log for configuration changes.
type WAL struct {
	path string
	mu   sync.Mutex
}

func NewWAL(path string) *WAL {
	return &WAL{path: path}
}

// Append adds a new commit to the log atomically.
func (w *WAL) Append(actor, taskID string, ops []PatchOp) (Commit, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	commit := Commit{
		ID:     fmt.Sprintf("commit_%s", uuid.New().String()[:8]),
		TS:     time.Now(),
		Actor:  actor,
		TaskID: taskID,
		Ops:    ops,
	}

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return commit, fmt.Errorf("wal: open failed: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(commit)
	if err != nil {
		return commit, fmt.Errorf("wal: marshal failed: %w", err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		return commit, fmt.Errorf("wal: write failed: %w", err)
	}

	return commit, f.Sync()
}

// Replay reads the log and applies all operations to the target.
func (w *WAL) Replay(target any) error {
	return w.ReplayUntil(target, "")
}

// ReplayUntil reads the log and applies operations until the given commitID is reached.
// If commitID is empty, it replays the entire log.
func (w *WAL) ReplayUntil(target any, commitID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.Open(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("wal: open failed: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// FIX (2026-09-02): bufio.Scanner defaults to a 64KB max token(line) size.
	// A single WAL commit can embed a full role/MCP/provider registration's
	// JSON payload (confirmed real: workspace/mcps/tushare.json alone is
	// ~334KB), which would exceed the default limit and make Scan() return
	// false with bufio.ErrTooLong -- ReplayUntil would then stop partway
	// through the log (any commits after the oversized one are never
	// replayed) and report an error, which loadLocked's caller (config.go)
	// currently treats as "quarantine the WAL and fall back to the last
	// snapshot" -- silently discarding every commit that was only recorded
	// in the WAL. Raise the buffer to comfortably exceed any single commit
	// this project has actually produced, with headroom.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		var commit Commit
		if err := json.Unmarshal(scanner.Bytes(), &commit); err != nil {
			return fmt.Errorf("wal: unmarshal failed at line %d: %w", lineNum, err)
		}

		if err := ApplyPatch(target, commit.Ops); err != nil {
			return fmt.Errorf("wal: apply failed at line %d (%s): %w", lineNum, commit.ID, err)
		}

		if commitID != "" && commit.ID == commitID {
			break
		}
	}

	return scanner.Err()
}

// LastCommitID returns the ID of the most recent commit in the log, or ""
// if the log is empty or doesn't exist yet. Used by persistLocked to embed
// a checkpoint marker in the snapshot it writes, so a future ReplaySince
// call knows which commits are already incorporated.
func (w *WAL) LastCommitID() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.Open(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lastID := ""
	for scanner.Scan() {
		var commit Commit
		if err := json.Unmarshal(scanner.Bytes(), &commit); err != nil {
			continue // tolerate a trailing corrupt/partial line; report the last good ID seen
		}
		lastID = commit.ID
	}
	return lastID, scanner.Err()
}

// ReplaySince applies WAL commits strictly AFTER the given commit ID,
// skipping sinceCommitID itself and everything before it. This is the
// crash-safe counterpart to Replay/ReplayUntil: a snapshot written by
// persistLocked records (in Config.WALCheckpoint) the last WAL commit it
// already reflects, so replaying only commits after that checkpoint avoids
// double-applying non-idempotent RFC 6902 "add"/"remove" ops on slices if
// the process crashed between the snapshot write and the matching WAL
// clear (see PersistForceSnapshot/Compactor.Compact).
//
// If sinceCommitID is empty (fresh install, or a snapshot written before
// this checkpoint mechanism existed), this behaves exactly like Replay.
// If sinceCommitID is non-empty but not found anywhere in the current log
// (e.g. the WAL was rotated/truncated since the checkpoint was recorded),
// this replays the ENTIRE log rather than silently skipping everything --
// erring toward re-applying over silently losing commits.
func (w *WAL) ReplaySince(target any, sinceCommitID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	effectiveSince := sinceCommitID
	if effectiveSince != "" {
		found, err := w.containsCommitLocked(effectiveSince)
		if err != nil {
			return err
		}
		if !found {
			effectiveSince = ""
		}
	}

	f, err := os.Open(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("wal: open failed: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	skipping := effectiveSince != ""
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		var commit Commit
		if err := json.Unmarshal(scanner.Bytes(), &commit); err != nil {
			return fmt.Errorf("wal: unmarshal failed at line %d: %w", lineNum, err)
		}
		if skipping {
			if commit.ID == effectiveSince {
				skipping = false
			}
			continue
		}
		if err := ApplyPatch(target, commit.Ops); err != nil {
			return fmt.Errorf("wal: apply failed at line %d (%s): %w", lineNum, commit.ID, err)
		}
	}
	return scanner.Err()
}

// containsCommitLocked scans the log for a commit with the given ID. Caller
// must already hold w.mu.
func (w *WAL) containsCommitLocked(commitID string) (bool, error) {
	f, err := os.Open(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var commit Commit
		if err := json.Unmarshal(scanner.Bytes(), &commit); err != nil {
			continue
		}
		if commit.ID == commitID {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// Clear truncates the WAL file. Used after successful compaction.
func (w *WAL) Clear() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Stats returns information about the WAL.
func (w *WAL) Stats() (int, int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.Open(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}

	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // FIX (2026-09-02): see ReplayUntil's comment
	for scanner.Scan() {
		count++
	}
	return count, info.Size(), nil
}
