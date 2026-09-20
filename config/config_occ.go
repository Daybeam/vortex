package config

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// currentSplitLayoutVersion is the highest SplitLayout.Version this binary
// knows how to read. If a config file on disk declares a newer version,
// this binary MUST refuse to load it rather than silently reading whatever
// fields still happen to parse and treating the rest (e.g. a Roles path it
// doesn't know about) as "no data".
//
// This silent-degradation failure mode is exactly what caused the
// 2026-07-23 production incident: an older binary loaded a config whose
// SplitLayout had gained a Roles field it didn't recognize, and silently
// ended up with an empty in-memory registry (MCPs/Roles both null) with no
// error anywhere. See orchestrator-mcp-go-debug-playbook-addendum-2026-07-23.md
// section 0 for the full incident writeup.
const currentSplitLayoutVersion = 2

// ErrUnsupportedSplitLayoutVersion is returned by checkSplitLayoutVersion
// (and therefore by Load()) when the on-disk config declares a SplitLayout
// version newer than currentSplitLayoutVersion.
var ErrUnsupportedSplitLayoutVersion = errors.New("config split layout version is newer than this binary supports")

// checkSplitLayoutVersion refuses to proceed if sl declares a version this
// binary doesn't understand. A nil layout (no split in use) always passes.
func checkSplitLayoutVersion(sl *SplitLayout) error {
	if sl == nil {
		return nil
	}
	if sl.Version > currentSplitLayoutVersion {
		return fmt.Errorf("%w: config declares split layout version %d, this binary only supports up to version %d -- refusing to load rather than silently treating unrecognized split sections as empty (rebuild/redeploy a version-aware binary first)",
			ErrUnsupportedSplitLayoutVersion, sl.Version, currentSplitLayoutVersion)
	}
	return nil
}

// setLoadedSnapshot / getLoadedSnapshot record the on-disk main config
// file's mtime and size as of the last successful Load(), for use by
// checkNotStaleForPersist. Guarded by their own mutex (loadedSnapshotMu)
// so Persist() -- which only ever takes Registry.Mu as an RLock -- can
// update this after a write without needing a lock upgrade, matching the
// existing splitLayout/splitLayoutMu pattern.
func (r *Registry) setLoadedSnapshot(modTime time.Time, size int64) {
	r.loadedSnapshotMu.Lock()
	defer r.loadedSnapshotMu.Unlock()
	r.loadedModTime = modTime
	r.loadedSize = size
}

func (r *Registry) getLoadedSnapshot() (time.Time, int64) {
	r.loadedSnapshotMu.RLock()
	defer r.loadedSnapshotMu.RUnlock()
	return r.loadedModTime, r.loadedSize
}

// occDisabled reports whether Persist()'s optimistic-concurrency check has
// been explicitly disabled via environment variable. Escape hatch for any
// deployment where this check turns out to be too strict in practice.
func occDisabled() bool {
	return os.Getenv("VORTEX_CONFIG_NO_OCC") == "1"
}

// ErrConfigChangedSinceLoad is returned by Persist() (via
// checkNotStaleForPersist) when the on-disk main config file has been
// modified -- by another process, or a manual hand-edit that StartWatcher
// hasn't picked up yet -- since this Registry last successfully Load()ed
// it. Callers should Load() (or wait for the next watcher poll) and retry,
// rather than blindly overwriting whatever changed on disk.
//
// This directly addresses the incident recorded in the 2026-06-29 addendum
// section 2: a register_role call's Persist() silently overwrote a
// concurrent manual edit to gitnexus's MCP config, because Persist() always
// writes the full in-memory state with no awareness that the on-disk file
// had moved on since the last Load().
var ErrConfigChangedSinceLoad = errors.New("config file changed on disk since last load; reload before persisting to avoid overwriting external changes")

// checkNotStaleForPersist implements the optimistic-concurrency check
// described above. A zero-value snapshot (never successfully loaded from
// an existing file -- fresh bootstrap, or the file didn't exist yet) always
// passes, since there is nothing to conflict with. A file that has vanished
// since load also passes, deliberately: that isn't the conflict case this
// check guards against, and blocking Persist() indefinitely in that case
// would be worse than just recreating the file.
func (r *Registry) checkNotStaleForPersist() error {
	if occDisabled() {
		return nil
	}
	loadedModTime, loadedSize := r.getLoadedSnapshot()
	if loadedModTime.IsZero() {
		return nil
	}
	info, err := os.Stat(r.configPath)
	if err != nil {
		return nil
	}
	if info.ModTime().After(loadedModTime) || info.Size() != loadedSize {
		return fmt.Errorf("%w (loaded mtime=%s size=%d, current on-disk mtime=%s size=%d)",
			ErrConfigChangedSinceLoad, loadedModTime, loadedSize, info.ModTime(), info.Size())
	}
	return nil
}

// persistAndSnapshot wraps persistSplitOrWhole (config_split.go) and, on a
// successful write, updates the loaded snapshot to reflect the just-written
// file state. Without this, the very next Persist() call in the same
// process would immediately see "changed since load" due to its own prior
// write (Persist() itself changes the file's mtime).
func (r *Registry) persistAndSnapshot(cfg Config) error {
	if err := r.persistSplitOrWhole(cfg); err != nil {
		return err
	}
	if info, statErr := os.Stat(r.configPath); statErr == nil {
		r.setLoadedSnapshot(info.ModTime(), info.Size())
	}
	return nil
}
