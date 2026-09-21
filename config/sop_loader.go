package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

// LoadSOPs reads all JSON files in the given directory and loads them into
// the registry's SOPs map.
//
// LOCKING CONTRACT (2026-07-07): unlike its original version, this function
// does NOT take r.Mu itself. The caller must already hold r.Mu (either the
// write lock, e.g. from within Registry.Load(), or -- if ever called
// standalone outside of Load() -- the caller is responsible for taking
// r.Mu.Lock() around this call). This mirrors how the adjacent skills-loading
// block in Load() operates directly on r.Skills under the lock that Load()
// already holds for its entire body.
//
// The previous version took r.Mu.Lock() internally, which meant that if this
// function were ever called from inside Load() (which holds r.Mu.Lock() for
// its whole body, matching the skills/MCPs/roles loading that already lives
// there), it would deadlock on the very first config load/reload -- sync.Mutex
// in Go is not re-entrant. This was caught before LoadSOPs had any call sites,
// so there is no existing caller relying on the old self-locking behavior.
func LoadSOPs(dir string, r *Registry) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // sops directory might not exist yet, this is fine
		}
		return err
	}

	keepSOPs := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read SOP %s: %w", entry.Name(), err)
		}

		var sop schemas.SOP
		if err := json.Unmarshal(data, &sop); err != nil {
			return fmt.Errorf("failed to parse SOP %s: %w", entry.Name(), err)
		}

		// FIX (2026-08-21): if ID is missing in JSON, default to filename (without .json)
		if sop.ID == "" {
			sop.ID = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		}

		r.SOPs[sop.ID] = &sop
		keepSOPs[sop.ID] = true
	}

	// FIX (2026-08-03): reconcile r.SOPs against disk -- same upsert-without-
	// reset issue as Registry.Load()'s other disk-driven maps (see pruneStale's
	// doc comment in config_prune.go). Only applied on the successful,
	// fully-completed path -- an error return above leaves r.SOPs untouched by
	// this call, matching this function's pre-existing partial-failure
	// semantics (a bad SOP file aborts the whole directory scan without
	// guaranteeing which entries were already applied).
	pruneStale(r.SOPs, keepSOPs)

	return nil
}
