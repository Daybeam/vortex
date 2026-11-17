package config

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/daybeam/vortex/schemas"
)

func (r *Registry) StartCompactor(ctx context.Context) {
	if r.compactor != nil {
		go r.compactor.Start(ctx)
	}
}

func (r *Registry) StartWatcher(ctx context.Context) {
	r.watcherWg.Add(1) // audit LEAK-2: track goroutine lifecycle
	go func() {
		defer r.watcherWg.Done() // audit LEAK-2: ensure StopWatcher can wait for exit
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "[config-watcher] goroutine panic: %v\n%s", r, debug.Stack())
			}
		}()
		info, err := os.Stat(r.configPath)
		var lastMod time.Time
		if err == nil {
			lastMod = info.ModTime()
		}

		// Also watch .env for hot-reload.
		// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md §change 5.
		envPath := filepath.Join(filepath.Dir(r.configPath), ".env")
		var envLastMod time.Time
		if envInfo, envErr := os.Stat(envPath); envErr == nil {
			envLastMod = envInfo.ModTime()
		}

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			// Watch config.json (existing)
			info, err := os.Stat(r.configPath)
			if err == nil && info.ModTime().After(lastMod) {
				lastMod = info.ModTime()
				if err := r.loadWithFallback(); err != nil {
					log.Printf("CRITICAL: config auto-reload failed: %v", err)
				}
			}

			// Watch .env — re-read into os.Environ, then reload config so
			// applySystemDefaults picks up the new env var values.
			if envInfo, envErr := os.Stat(envPath); envErr == nil && envInfo.ModTime().After(envLastMod) {
				envLastMod = envInfo.ModTime()
				r.Mu.RLock()
				hook := r.envReloadHook
				r.Mu.RUnlock()
				if hook != nil {
					hook()
				}
				if err := r.loadWithFallback(); err != nil {
					log.Printf("CRITICAL: config auto-reload after .env change failed: %v", err)
				}
			}
		}
	}()
}

// StopWatcher waits for the config watcher goroutine to fully exit after
// its context has been cancelled (audit LEAK-2: was goroutine leak — caller
// had no way to know when the watcher actually stopped).
func (r *Registry) StopWatcher() {
	r.watcherWg.Wait()
}

func (r *Registry) Persist() error {
	// audit P-MED-5: release RLock before file write I/O — previously held
	// RLock for the entire persistLocked() call (including marshal + file
	// write), blocking all writers for the full I/O duration. Now we clone
	// the config snapshot under RLock, release, then write without any lock.
	r.Mu.RLock()
	if err := r.checkNotStaleForPersist(); err != nil {
		r.Mu.RUnlock()
		return err
	}
	cfg := r.buildConfigSnapshotLocked()
	r.Mu.RUnlock()
	return r.persistAndSnapshot(cfg)
}

func (r *Registry) persistLocked() error {
	if err := r.checkNotStaleForPersist(); err != nil {
		return err
	}
	cfg := r.buildConfigSnapshotLocked()
	// NOTE: deliberately not writing r.walCheckpoint here -- persistLocked is
	// called under varying lock types (Persist() holds only an RLock;
	// PersistForceSnapshot/Compactor.Compact hold a full Lock), so mutating a
	// Registry field here would be unsafe when only an RLock is held. This is
	// fine: r.walCheckpoint is only ever consulted once, in NewRegistry right
	// after loadLocked runs (itself always under a full Lock), so there is no
	// same-process reader that needs this update to be reflected immediately.
	return r.persistAndSnapshot(cfg)
}

// buildConfigSnapshotLocked clones all config data into a Config struct.
// Caller MUST hold at least r.Mu.RLock().
func (r *Registry) buildConfigSnapshotLocked() Config {
	providers := make(map[string]ProviderConfig)
	for k, v := range r.Providers {
		providers[k] = *v
	}
	skills := make([]Skill, 0)
	for id, s := range r.Skills {
		if id == "" || s.ID == "" {
			continue // FIX (2026-09-03): restored ghost-entry filtering on write-back, dropped during the loader.go split
		}
		skills = append(skills, *s)
	}
	mcps := make([]MCPDef, 0)
	for id, m := range r.MCPs {
		if id == "" || m.ID == "" {
			continue
		}
		mcps = append(mcps, *m)
	}
	roles := make([]Role, 0)
	for id, role := range r.Roles {
		if id == "" || role.ID == "" {
			continue
		}
		roles = append(roles, *role)
	}
	roleGroups := make([]RoleGroup, 0)
	for id, g := range r.RoleGroups {
		if id == "" || g.ID == "" {
			continue
		}
		roleGroups = append(roleGroups, *g)
	}
	sops := make([]schemas.SOP, 0)
	for id, s := range r.SOPs {
		if id == "" || s.ID == "" {
			continue
		}
		sops = append(sops, *s)
	}

	// FIX (2026-09-07): record which WAL commit (if any) this snapshot
	// already reflects. r's in-memory state always includes every commit
	// appended so far (CommitOps applies to r synchronously before/alongside
	// the WAL append), so "the current last WAL commit" is always safe to
	// record as this snapshot's checkpoint -- see wal.go's ReplaySince.
	walCheckpoint := ""
	if r.wal != nil {
		if id, err := r.wal.LastCommitID(); err == nil {
			walCheckpoint = id
		}
	}

	return Config{
		DefaultProvider:        r.DefaultProvider,
		GlobalFallback:         r.GlobalFallback,
		Providers:              providers,
		Skills:                 skills,
		MCPs:                   mcps,
		Roles:                  roles,
		RoleGroups:             roleGroups,
		SOPs:                   sops,
		ExternalRuntimes:       r.ExternalRuntimes,
		System:                 r.System,
		EnableDynamicRoleGen:   r.EnableDynamicRoleGen,
		EnableEphemeralRoleGen: r.EnableEphemeralRoleGen,
		SwarmModeEnabled:       r.SwarmModeEnabled,
		RoleCookbookSource:     r.RoleCookbookSource,
		RoleCookbookSources:    r.RoleCookbookSources,
		SkillsDir:              r.SkillsDir,
		RolesDir:               r.RolesDir,
		WALCheckpoint:          walCheckpoint,
	}
}
