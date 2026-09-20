package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StagingFileMeta records a tracked file's identity within a StagedWorkspace
// snapshot: its path (relative to the task's workspace directory), content
// hash, and size. See docs/STAGED_WORKSPACE_DESIGN.md §6.1.
type StagingFileMeta struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// StagingJournalEntry is one append-only record of a step's effect on the
// task workspace: what changed between the pre-step and post-step snapshot,
// and whether the step's result was accepted or rolled back. See
// docs/STAGED_WORKSPACE_DESIGN.md §6.1.
type StagingJournalEntry struct {
	StepID         string            `json:"step_id"`
	TaskID         string            `json:"task_id"`
	SnapshotBefore string            `json:"snapshot_before,omitempty"`
	SnapshotAfter  string            `json:"snapshot_after,omitempty"`
	Status         string            `json:"status"`
	Created        []StagingFileMeta `json:"created,omitempty"`
	Modified       []StagingFileMeta `json:"modified,omitempty"`
	Deleted        []StagingFileMeta `json:"deleted,omitempty"`
	DiffSummary    string            `json:"diff_summary,omitempty"`
	Verified       bool              `json:"verified"`
	RolledBack     bool              `json:"rolled_back"`
	Timestamp      time.Time         `json:"timestamp"`
}

// SnapshotIndex is the full set of tracked file states at one point in
// time (e.g. immediately before or after a step executes). See
// docs/STAGED_WORKSPACE_DESIGN.md §6.2.
type SnapshotIndex struct {
	ID      string                     `json:"id"`
	Files   map[string]StagingFileMeta `json:"files"`
	Created time.Time                  `json:"created"`
}

// StagedWorkspace implements the minimal-viable version of the design in
// docs/STAGED_WORKSPACE_DESIGN.md: step-boundary content-addressable
// snapshotting, append-only journaling, and rollback-on-failure for one
// task's workspace directory.
//
// SCOPE NOTE (2026-08-21): this tracks the task's own output directory
// (the same directory AssetManager side-loads into and persistGraph writes
// manifest.json/artifacts.json to), NOT an isolated `.staging/workspace/`
// copy that tool calls are redirected into. Redirecting every MCP tool
// call's file writes into an isolated per-task working directory (design
// doc §5.2/§9.4) is a materially larger, riskier change touching the MCP
// client dispatch layer across every bound MCP, and is left for a future,
// dedicated follow-up per the design doc's own §10 framing ("A minimal
// viable version can be implemented with step-boundary scans and
// journaling only"). What IS tracked and real today: any file a step
// actually writes under its own task directory -- side-loaded JSON via
// AssetManager.Handle, or any output_ref path the model reports that
// happens to resolve under the task directory -- is snapshotted, diffed,
// and can be rolled back.
type StagedWorkspace struct {
	TaskDir      string // e.g. outputBase/<taskID>
	SnapshotsDir string // TaskDir/.staging/snapshots
	JournalPath  string // TaskDir/.staging/journal.json

	journalMu sync.Mutex
}

// stagingExcludeNames are top-level files inside TaskDir that
// StagedWorkspace never tracks: the pre-existing manifest/artifact files
// that persistGraph already manages independently. The `.staging`
// directory itself is always excluded regardless of nesting depth.
var stagingExcludeNames = map[string]bool{
	"manifest.json":  true,
	"artifacts.json": true,
}

// NewStagedWorkspace prepares (and creates, if missing) the `.staging`
// layout under taskDir. Cheap to call repeatedly; safe to construct fresh
// per call site rather than caching.
func NewStagedWorkspace(taskDir string) *StagedWorkspace {
	sw := &StagedWorkspace{
		TaskDir:      taskDir,
		SnapshotsDir: filepath.Join(taskDir, ".staging", "snapshots"),
		JournalPath:  filepath.Join(taskDir, ".staging", "journal.json"),
	}
	_ = os.MkdirAll(sw.SnapshotsDir, 0755)
	return sw
}

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// scanWorkspace walks TaskDir recursively (excluding `.staging/` at any
// depth, and the top-level manifest/artifact files) and returns a map of
// slash-separated relative path -> StagingFileMeta for every tracked file.
func (sw *StagedWorkspace) scanWorkspace() (map[string]StagingFileMeta, error) {
	out := make(map[string]StagingFileMeta)

	var walk func(dir, relPrefix string) error
	walk = func(dir, relPrefix string) error {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if e.Name() == ".staging" {
				continue
			}
			rel := e.Name()
			if relPrefix != "" {
				rel = relPrefix + "/" + e.Name()
			}
			full := filepath.Join(dir, e.Name())
			if e.IsDir() {
				if err := walk(full, rel); err != nil {
					return err
				}
				continue
			}
			if relPrefix == "" && stagingExcludeNames[e.Name()] {
				continue
			}
			data, rerr := os.ReadFile(full)
			if rerr != nil {
				// Best-effort: a file mid-write by a concurrent process
				// shouldn't abort the whole scan.
				continue
			}
			out[rel] = StagingFileMeta{Path: rel, SHA256: hashBytes(data), Size: int64(len(data))}
		}
		return nil
	}

	if err := walk(sw.TaskDir, ""); err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	return out, nil
}

// snapshotContent writes data to CAS under its own hash if not already
// present. Idempotent and safe to call redundantly -- duplicate content
// across files or across steps is only ever stored once (design doc §3.2).
func (sw *StagedWorkspace) snapshotContent(hash string, data []byte) error {
	dst := filepath.Join(sw.SnapshotsDir, hash+".bin")
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func (sw *StagedWorkspace) readSnapshot(hash string) ([]byte, error) {
	return os.ReadFile(filepath.Join(sw.SnapshotsDir, hash+".bin"))
}

// snapshotAll scans the current workspace, stores every tracked file's
// content in CAS, and returns the resulting SnapshotIndex.
func (sw *StagedWorkspace) snapshotAll(id string) (*SnapshotIndex, error) {
	files, err := sw.scanWorkspace()
	if err != nil {
		return nil, err
	}
	for rel, meta := range files {
		full, err := safeJoin(sw.TaskDir, rel)
		if err != nil {
			return nil, fmt.Errorf("staged_workspace: snapshot %s: %w", rel, err)
		}
		data, rerr := os.ReadFile(full)
		if rerr != nil {
			continue
		}
		if err := sw.snapshotContent(meta.SHA256, data); err != nil {
			return nil, fmt.Errorf("staged_workspace: snapshot %s: %w", rel, err)
		}
	}
	return &SnapshotIndex{ID: id, Files: files, Created: time.Now()}, nil
}

// TakePreStepSnapshot scans and snapshots the current workspace state
// before a step executes (design doc §5.1). The returned SnapshotIndex is
// the reference point for RecordPostStepSnapshot's diff and for Rollback.
func (sw *StagedWorkspace) TakePreStepSnapshot(stepID string) (*SnapshotIndex, error) {
	return sw.snapshotAll(fmt.Sprintf("pre_%s_%d", stepID, time.Now().UnixNano()))
}

// RecordPostStepSnapshot re-scans the workspace after a step executes
// (design doc §5.3), computes the delta against pre (created/modified/
// deleted files), snapshots any new content, and appends a journal entry
// recording the result. Pass pre=nil if no pre-step snapshot was taken --
// the diff then treats every currently-tracked file as "created".
func (sw *StagedWorkspace) RecordPostStepSnapshot(taskID, stepID, status string, pre *SnapshotIndex) (*StagingJournalEntry, *SnapshotIndex, error) {
	post, err := sw.snapshotAll(fmt.Sprintf("post_%s_%d", stepID, time.Now().UnixNano()))
	if err != nil {
		return nil, nil, err
	}

	var preFiles map[string]StagingFileMeta
	preID := ""
	if pre != nil {
		preFiles = pre.Files
		preID = pre.ID
	}

	entry := &StagingJournalEntry{
		StepID:         stepID,
		TaskID:         taskID,
		SnapshotBefore: preID,
		SnapshotAfter:  post.ID,
		Status:         status,
		Timestamp:      time.Now(),
	}

	for rel, meta := range post.Files {
		prevMeta, existed := preFiles[rel]
		switch {
		case !existed:
			entry.Created = append(entry.Created, meta)
		case prevMeta.SHA256 != meta.SHA256:
			entry.Modified = append(entry.Modified, meta)
		}
	}
	for rel, meta := range preFiles {
		if _, stillThere := post.Files[rel]; !stillThere {
			entry.Deleted = append(entry.Deleted, meta)
		}
	}
	entry.DiffSummary = fmt.Sprintf("+%d created, ~%d modified, -%d deleted",
		len(entry.Created), len(entry.Modified), len(entry.Deleted))

	if err := sw.AppendJournalEntry(entry); err != nil {
		return entry, post, err
	}
	return entry, post, nil
}

// Rollback restores the tracked workspace to exactly the state recorded in
// pre (design doc §5.5): files present in pre are restored from CAS
// (overwriting any current content), and files present now but absent from
// pre are deleted.
func (sw *StagedWorkspace) Rollback(pre *SnapshotIndex) error {
	if pre == nil {
		return fmt.Errorf("staged_workspace: cannot roll back without a pre-step snapshot")
	}
	current, err := sw.scanWorkspace()
	if err != nil {
		return err
	}
	for rel, meta := range pre.Files {
		full, jerr := safeJoin(sw.TaskDir, rel)
		if jerr != nil {
			return fmt.Errorf("staged_workspace: rollback %s: %w", rel, jerr)
		}
		if curMeta, ok := current[rel]; ok && curMeta.SHA256 == meta.SHA256 {
			continue // already matches, nothing to restore
		}
		data, rerr := sw.readSnapshot(meta.SHA256)
		if rerr != nil {
			return fmt.Errorf("staged_workspace: rollback missing snapshot content for %s (hash %s): %w", rel, meta.SHA256, rerr)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0644); err != nil {
			return err
		}
	}
	for rel := range current {
		if _, keep := pre.Files[rel]; !keep {
			full, jerr := safeJoin(sw.TaskDir, rel)
			if jerr != nil {
				continue // skip paths that escape the task directory
			}
			_ = os.Remove(full)
		}
	}
	return nil
}

// AppendJournalEntry appends one entry to journal.json (read-modify-write
// under a mutex -- the file itself is small and step-frequency writes, so
// this is not a performance concern; the data model it presents is
// logically append-only per design doc §3.3).
func (sw *StagedWorkspace) AppendJournalEntry(entry *StagingJournalEntry) error {
	sw.journalMu.Lock()
	defer sw.journalMu.Unlock()

	entries, err := sw.loadJournalLocked()
	if err != nil {
		return err
	}
	entries = append(entries, *entry)
	return sw.writeJournalLocked(entries)
}

// MarkRolledBack finds the most recent journal entry for stepID and marks
// it RolledBack=true. Call this after a successful Rollback.
func (sw *StagedWorkspace) MarkRolledBack(stepID string) error {
	sw.journalMu.Lock()
	defer sw.journalMu.Unlock()

	entries, err := sw.loadJournalLocked()
	if err != nil {
		return err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].StepID == stepID {
			entries[i].RolledBack = true
			break
		}
	}
	return sw.writeJournalLocked(entries)
}

func (sw *StagedWorkspace) loadJournalLocked() ([]StagingJournalEntry, error) {
	data, err := os.ReadFile(sw.JournalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []StagingJournalEntry{}, nil
		}
		return nil, err
	}
	var entries []StagingJournalEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (sw *StagedWorkspace) writeJournalLocked(entries []StagingJournalEntry) error {
	if err := os.MkdirAll(filepath.Dir(sw.JournalPath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := sw.JournalPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, sw.JournalPath)
}

// LoadJournal returns every recorded journal entry for this workspace, in
// append order.
func (sw *StagedWorkspace) LoadJournal() ([]StagingJournalEntry, error) {
	sw.journalMu.Lock()
	defer sw.journalMu.Unlock()
	return sw.loadJournalLocked()
}

// CleanupStaging removes this workspace's `.staging` bookkeeping if
// TaskDir's most recent modification is older than ttl (design doc §8.4).
// keepJournal, when true, removes only the snapshots subdirectory and
// leaves journal.json in place -- used for failed/aborted tasks, which the
// design doc recommends retaining longer for debugging (§8.3).
func (sw *StagedWorkspace) CleanupStaging(ttl time.Duration, keepJournal bool) error {
	info, err := os.Stat(sw.TaskDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if time.Since(info.ModTime()) < ttl {
		return nil
	}
	if keepJournal {
		return os.RemoveAll(sw.SnapshotsDir)
	}
	return os.RemoveAll(filepath.Join(sw.TaskDir, ".staging"))
}
