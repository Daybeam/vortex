package config

// pruneStale removes any key from m that is not present in keep.
//
// BACKGROUND (2026-08-03): Registry.Load() and LoadSOPs previously only ever
// upserted entries into r.Providers/r.Skills/r.MCPs/r.Roles/r.RoleGroups/r.SOPs
// from whatever was currently on disk -- an item removed from disk (by an
// unregister_* action deleting a split-layout file, or by a manual edit) was
// never removed from the in-memory map by any subsequent Load()/reload(),
// because these maps are allocated once in NewRegistry and never reset
// inside Load() itself. This meant "delete" operations only ever appeared to
// work (the action returns {"ok": true} and the deleted item's own file may
// or may not be removed), while the in-memory registry -- and hence every
// read path built on it (get_config, discover, submit_task role/mcp
// resolution) -- kept serving the stale, supposedly-deleted entry forever,
// until the whole process was restarted from a fresh Registry.
//
// Call pruneStale once per disk-driven map, immediately after the loop that
// populates it, passing the set of IDs actually seen in this Load() pass.
// Do NOT call this for maps that are not purely disk-driven (e.g.
// r.DynamicMCPs, which is populated by runtime JIT/dynamic-MCP registration
// and has no corresponding on-disk source to reconcile against).
func pruneStale[V any](m map[string]V, keep map[string]bool) {
	for id := range m {
		if !keep[id] {
			delete(m, id)
		}
	}
}
