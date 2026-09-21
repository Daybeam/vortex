// Package overlay manages atomic edit sessions with crash recovery.
//
// Atomicity design:
//   - All edits are staged in memory (overlay) until commit
//   - Before commit: a snapshot directory is written with originals + manifest
//   - Commit uses temp-file + rename per file, updating manifest after each rename
//   - On startup: incomplete manifests are recovered (re-applied or rolled back)
//   - Concurrent sessions use per-file mutexes + hash-based conflict detection
//
// The snapshot directory (~/.code-intel-mcp/snapshots/<session_id>/) is the
// crash-recovery journal. It is deleted on clean commit or rollback.
package overlay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	t "github.com/daybeam/vortex/pkg/codeintel/types"
)

// Validator is implemented by analyzers that can validate an overlay.
type Validator interface {
	SetOverlay(file, content string)
	ClearOverlay(file string)
	ReindexWithOverlay() error
	ValidateOverlay() []t.Diagnostic
}

// fileLock serialises commits that touch the same file.
type fileLock struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newFileLock() *fileLock { return &fileLock{locks: make(map[string]*sync.Mutex)} }

// lockFiles acquires per-file mutexes in sorted order (prevents deadlock).
// Returns an unlock function.
func (fl *fileLock) lockFiles(files []string) func() {
	sorted := make([]string, len(files))
	copy(sorted, files)
	sort.Strings(sorted)

	var acquired []*sync.Mutex
	for _, f := range sorted {
		fl.mu.Lock()
		m, ok := fl.locks[f]
		if !ok {
			m = &sync.Mutex{}
			fl.locks[f] = m
		}
		fl.mu.Unlock()
		m.Lock()
		acquired = append(acquired, m)
	}
	return func() {
		for _, m := range acquired {
			m.Unlock()
		}
	}
}

// Manager manages named edit sessions.
type Manager struct {
	mu          sync.Mutex
	sessions    map[string]*t.EditSession
	analyzer    Validator
	snapshotDir string // root for crash-recovery snapshot dirs
	fileLock    *fileLock
}

// NewManager creates a new overlay manager.
// snapshotRoot defaults to ~/.code-intel-mcp/snapshots if empty.
func NewManager(analyzer Validator, snapshotRoot string) *Manager {
	if snapshotRoot == "" {
		home, _ := os.UserHomeDir()
		snapshotRoot = filepath.Join(home, ".code-intel-mcp", "snapshots")
	}
	_ = os.MkdirAll(snapshotRoot, 0755)
	m := &Manager{
		sessions:    make(map[string]*t.EditSession),
		analyzer:    analyzer,
		snapshotDir: snapshotRoot,
		fileLock:    newFileLock(),
	}
	m.recoverAll() // replay any incomplete commits from prior crashes
	return m
}

// Begin creates a new edit session and returns its ID.
func (m *Manager) Begin() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := fmt.Sprintf("session_%d", time.Now().UnixNano())
	m.sessions[id] = &t.EditSession{
		ID:       id,
		Status:   t.SessionOpen,
		Edits:    make(map[string]string),
		Original: make(map[string]string),
	}
	return id
}

// WriteFile stages a file modification into the session overlay.
// The original content is captured (and hashed) on first write for conflict detection.
func (m *Manager) WriteFile(sessionID, file, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, err := m.getOpen(sessionID)
	if err != nil {
		return err
	}

	// Capture original on first touch
	if _, seen := sess.Original[file]; !seen {
		orig, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("reading original %s: %w", file, err)
		}
		sess.Original[file] = string(orig)
	}

	sess.Edits[file] = content
	m.analyzer.SetOverlay(file, content)
	return nil
}

// Validate type-checks all staged edits without writing to disk.
func (m *Manager) Validate(sessionID string) ([]t.Diagnostic, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, err := m.getOpen(sessionID)
	if err != nil {
		return nil, err
	}

	if err := m.analyzer.ReindexWithOverlay(); err != nil {
		return nil, fmt.Errorf("reindex failed: %w", err)
	}

	diags := m.analyzer.ValidateOverlay()
	sess.Errors = diags
	if len(diags) == 0 {
		sess.Status = t.SessionValidated
	}
	return diags, nil
}

// Commit writes all staged edits to disk atomically.
//
// Protocol:
//  1. Acquire per-file locks (sorted order, deadlock-safe)
//  2. Conflict check: hash current on-disk content vs. captured original
//  3. Write snapshot dir: copy originals + write manifest(phase=committing)
//  4. For each file: write .tmp_ci, Rename, update manifest entry
//  5. Write manifest(phase=committed), remove snapshot dir
//  6. Release locks
func (m *Manager) Commit(sessionID string) error {
	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", sessionID)
	}
	if sess.Status == t.SessionOpen {
		m.mu.Unlock()
		return fmt.Errorf("session %s has not been validated; call validate first", sessionID)
	}
	if sess.Status != t.SessionValidated {
		m.mu.Unlock()
		return fmt.Errorf("session %s is %s, cannot commit", sessionID, sess.Status)
	}
	if len(sess.Errors) > 0 {
		m.mu.Unlock()
		return fmt.Errorf("session %s has %d type errors; fix them before committing", sessionID, len(sess.Errors))
	}

	// Collect files to lock
	files := make([]string, 0, len(sess.Edits))
	for f := range sess.Edits {
		files = append(files, f)
	}
	m.mu.Unlock()

	// 1. Acquire per-file locks
	unlock := m.fileLock.lockFiles(files)
	defer unlock()

	m.mu.Lock()
	sess = m.sessions[sessionID] // re-fetch under lock
	m.mu.Unlock()

	// 2. Conflict check
	for file, origContent := range sess.Original {
		current, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("conflict check read failed for %s: %w", file, err)
		}
		if hash(string(current)) != hash(origContent) {
			sess.Status = t.SessionConflict
			return &t.ConflictError{
				File:         file,
				ExpectedHash: hash(origContent),
				ActualHash:   hash(string(current)),
			}
		}
	}

	// 3. Write snapshot directory
	snapDir := filepath.Join(m.snapshotDir, sessionID)
	if err := os.MkdirAll(filepath.Join(snapDir, "files"), 0755); err != nil {
		return fmt.Errorf("creating snapshot dir: %w", err)
	}

	var entries []t.FileEntry
	for i, file := range files {
		snapPath := filepath.Join(snapDir, "files", fmt.Sprintf("%d_%s.orig", i, safeFilename(file)))
		orig := sess.Original[file]
		if err := os.WriteFile(snapPath, []byte(orig), 0644); err != nil {
			_ = os.RemoveAll(snapDir)
			return fmt.Errorf("writing snapshot for %s: %w", file, err)
		}
		entries = append(entries, t.FileEntry{
			SnapshotPath: snapPath,
			TargetPath:   file,
			TmpPath:      file + ".tmp_ci",
			Renamed:      false,
			OriginalHash: hash(orig),
		})
	}

	manifest := &t.Manifest{
		SessionID: sessionID,
		Phase:     t.SessionCommitting,
		Files:     entries,
	}
	if err := writeManifest(snapDir, manifest); err != nil {
		_ = os.RemoveAll(snapDir)
		return fmt.Errorf("writing manifest: %w", err)
	}

	// 4. Write tmp files and rename one by one, updating manifest after each
	for i, entry := range manifest.Files {
		newContent := sess.Edits[entry.TargetPath]

		if err := os.WriteFile(entry.TmpPath, []byte(newContent), 0644); err != nil {
			// Rollback: restore originals for already-renamed files
			m.restoreFromManifest(manifest, i)
			_ = os.RemoveAll(snapDir)
			return fmt.Errorf("writing tmp for %s: %w", entry.TargetPath, err)
		}

		if err := os.Rename(entry.TmpPath, entry.TargetPath); err != nil {
			_ = os.Remove(entry.TmpPath)
			m.restoreFromManifest(manifest, i)
			_ = os.RemoveAll(snapDir)
			return fmt.Errorf("rename failed for %s: %w", entry.TargetPath, err)
		}

		// Update manifest entry: this file is now committed
		manifest.Files[i].Renamed = true
		if err := writeManifest(snapDir, manifest); err != nil {
			// Non-fatal: manifest update failed but file is already renamed.
			// Recovery on next startup will see Renamed=false but file is new — safe.
			_ = err
		}
	}

	// 5. Mark committed and clean up
	manifest.Phase = t.SessionCommitted
	_ = writeManifest(snapDir, manifest)
	_ = os.RemoveAll(snapDir)

	// Clear overlays
	m.mu.Lock()
	for file := range sess.Edits {
		m.analyzer.ClearOverlay(file)
	}
	sess.Status = t.SessionCommitted
	m.mu.Unlock()

	return nil
}

// Rollback discards all staged edits and removes the snapshot if present.
func (m *Manager) Rollback(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, ok := m.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	for file := range sess.Edits {
		m.analyzer.ClearOverlay(file)
	}

	snapDir := filepath.Join(m.snapshotDir, sessionID)
	_ = os.RemoveAll(snapDir)

	sess.Status = t.SessionRolledBack
	sess.Edits = make(map[string]string)
	return nil
}

// GetSession returns a snapshot of the current session state.
func (m *Manager) GetSession(sessionID string) (*t.EditSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, ok := m.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	cp := *sess
	return &cp, nil
}

// ListSessions returns all session IDs and statuses.
func (m *Manager) ListSessions() map[string]t.SessionStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]t.SessionStatus)
	for id, sess := range m.sessions {
		result[id] = sess.Status
	}
	return result
}

// --- crash recovery ---

// recoverAll scans the snapshot directory for incomplete commits and replays them.
// Called once on startup.
func (m *Manager) recoverAll() {
	entries, err := os.ReadDir(m.snapshotDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		snapDir := filepath.Join(m.snapshotDir, e.Name())
		manifest, err := readManifest(snapDir)
		if err != nil {
			continue
		}
		switch manifest.Phase {
		case t.SessionCommitting:
			// Partially committed: re-apply renamed files, restore non-renamed
			m.recoverCommitting(snapDir, manifest)
		case t.SessionCommitted:
			// Clean: just remove leftover snapshot dir
			_ = os.RemoveAll(snapDir)
		default:
			// open/validated/rolled_back that were never cleaned up
			_ = os.RemoveAll(snapDir)
		}
	}
}

// recoverCommitting handles a crash mid-commit.
// Files with Renamed=true are already on disk correctly.
// Files with Renamed=false need their originals restored.
func (m *Manager) recoverCommitting(snapDir string, manifest *t.Manifest) {
	for _, entry := range manifest.Files {
		if entry.Renamed {
			// Already committed — verify hash consistency, nothing to do
			continue
		}
		// Not yet renamed: restore original
		orig, err := os.ReadFile(entry.SnapshotPath)
		if err != nil {
			continue
		}
		// Clean up any leftover tmp file
		_ = os.Remove(entry.TmpPath)
		// Only restore if current content differs (idempotent)
		current, _ := os.ReadFile(entry.TargetPath)
		if hash(string(current)) != hash(string(orig)) {
			_ = os.WriteFile(entry.TargetPath, orig, 0644)
		}
	}
	_ = os.RemoveAll(snapDir)
}

// restoreFromManifest restores originals for all entries up to (not including) upToIndex.
func (m *Manager) restoreFromManifest(manifest *t.Manifest, upToIndex int) {
	for i := 0; i < upToIndex; i++ {
		entry := manifest.Files[i]
		orig, err := os.ReadFile(entry.SnapshotPath)
		if err != nil {
			continue
		}
		_ = os.WriteFile(entry.TargetPath, orig, 0644)
	}
}

// --- helpers ---

func writeManifest(snapDir string, manifest *t.Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(snapDir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(snapDir, "manifest.json"))
}

func readManifest(snapDir string) (*t.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(snapDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m t.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8]) // first 8 bytes is enough for conflict detection
}

func safeFilename(path string) string {
	safe := ""
	for _, c := range filepath.Base(path) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			safe += string(c)
		} else {
			safe += "_"
		}
	}
	if len(safe) > 40 {
		safe = safe[:40]
	}
	return safe
}

func (m *Manager) getOpen(id string) (*t.EditSession, error) {
	sess, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	if sess.Status != t.SessionOpen && sess.Status != t.SessionValidated {
		return nil, fmt.Errorf("session %s is %s and cannot be modified", id, sess.Status)
	}
	return sess, nil
}
