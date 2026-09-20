package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

const (
	defaultSplitThresholdBytes = 100 * 1024 // Split whole file if > 100KB
	itemSplitThresholdBytes    = 20 * 1024  // Split individual item to its own file if > 20KB
	countSplitThreshold        = 20         // Split section to directory if > 20 items
)

type SplitLayout struct {
	Version    int    `json:"version"`
	MCPs       string `json:"mcps,omitempty"`
	Roles      string `json:"roles,omitempty"`
	RoleGroups string `json:"role_groups,omitempty"`
	Skills     string `json:"skills,omitempty"` // ADDED (2026-08-16)
	SOPs       string `json:"sops,omitempty"`   // ADDED (2026-08-31)
}

func splitThresholdBytes() int {
	if v := os.Getenv("VORTEX_CONFIG_SPLIT_THRESHOLD"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return defaultSplitThresholdBytes
}

func autoSplitDisabled() bool {
	return os.Getenv("VORTEX_CONFIG_NO_AUTOSPLIT") == "1"
}
func (r *Registry) getSplitLayout() *SplitLayout {
	r.splitLayoutMu.RLock()
	defer r.splitLayoutMu.RUnlock()
	return r.splitLayout
}

func (r *Registry) setSplitLayout(sl *SplitLayout) {
	r.splitLayoutMu.Lock()
	defer r.splitLayoutMu.Unlock()
	r.splitLayout = sl
}
func loadSplitFiles(configPath string, sl *SplitLayout, cfg *Config) {
	if sl == nil {
		return
	}
	dir := ConfigDir(configPath)

	loadSection(filepath.Join(dir, sl.MCPs), &cfg.MCPs)
	loadSection(filepath.Join(dir, sl.Roles), &cfg.Roles)
	loadSection(filepath.Join(dir, sl.RoleGroups), &cfg.RoleGroups)
	loadSection(filepath.Join(dir, sl.SOPs), &cfg.SOPs)
}

func loadSection[T any](path string, target *[]T) {
	if path == "" || strings.HasSuffix(path, string(filepath.Separator)) || strings.HasSuffix(path, "/") {
		// If it's a directory (or intended to be), we'll handle it below.
		// Actually, filepath.Join with empty string doesn't help much.
	}

	// Check if path exists
	info, err := os.Stat(path)
	if err != nil {
		return
	}

	if info.IsDir() {
		// Load all .json files from directory
		files, err := os.ReadDir(path)
		if err != nil {
			log.Printf("warning: failed to read split config directory %s: %v", path, err)
			return
		}
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(strings.ToLower(f.Name()), ".json") {
				var item T
				data, err := os.ReadFile(filepath.Join(path, f.Name()))
				if err != nil {
					continue
				}
				if err := json.Unmarshal(data, &item); err == nil {
					*target = append(*target, item)
				}
			}
		}
	} else {
		// Load from single file
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var items []T
		if err := json.Unmarshal(data, &items); err == nil {
			*target = append(*target, items...)
		} else {
			// Maybe it's a single item in a split file?
			// (Though usually split files are arrays)
			var item T
			if err := json.Unmarshal(data, &item); err == nil {
				*target = append(*target, item)
			} else {
				log.Printf("warning: failed to parse split config file %s: %v", path, err)
			}
		}
	}
}
func (r *Registry) persistSplitOrWhole(cfg Config) error {
	existing := r.getSplitLayout()

	whole, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	if existing != nil {
		return r.writeSplit(cfg, existing)
	}

	if autoSplitDisabled() || len(whole) <= splitThresholdBytes() {
		return atomicWriteFile(r.configPath, whole, 0644)
	}

	// Default split layout for new migrations
	sl := &SplitLayout{
		Version:    2,
		MCPs:       "workspace/mcps/",
		Roles:      "workspace/roles/",
		RoleGroups: "workspace/role_groups/",
		Skills:     "workspace/skills/",
		SOPs:       "workspace/sops/",
	}
	return r.writeSplit(cfg, sl)
}

func (r *Registry) writeSplit(cfg Config, sl *SplitLayout) error {
	dir := ConfigDir(r.configPath)

	if err := writeSection(dir, sl.MCPs, cfg.MCPs, func(m MCPDef) string { return m.ID }); err != nil {
		return err
	}
	if err := writeSection(dir, sl.Roles, cfg.Roles, func(role Role) string { return role.ID }); err != nil {
		return err
	}
	if err := writeSection(dir, sl.RoleGroups, cfg.RoleGroups, func(g RoleGroup) string { return g.ID }); err != nil {
		return err
	}
	if err := writeSection(dir, sl.Skills, cfg.Skills, func(s Skill) string { return s.ID }); err != nil {
		return err
	}
	if err := writeSection(dir, sl.SOPs, cfg.SOPs, func(s schemas.SOP) string { return s.ID }); err != nil {
		return err
	}

	mainCfg := cfg
	mainCfg.SplitLayout = sl
	mainCfg.MCPs = nil
	mainCfg.Roles = nil
	mainCfg.RoleGroups = nil
	mainCfg.Skills = nil
	mainCfg.SOPs = nil

	mainData, err := json.MarshalIndent(mainCfg, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(r.configPath, mainData, 0644); err != nil {
		return fmt.Errorf("writing main config file: %w", err)
	}

	r.setSplitLayout(sl)
	return nil
}

func writeSection[T any](baseDir, sectionPath string, items []T, getID func(T) string) error {
	if sectionPath == "" {
		return nil
	}

	fullPath := filepath.Join(baseDir, sectionPath)
	isDir := strings.HasSuffix(sectionPath, "/") || strings.HasSuffix(sectionPath, string(filepath.Separator))

	if !isDir && (len(items) > countSplitThreshold) {
		// Auto-promote to directory if it's getting too big
		isDir = true
		sectionPath = sectionPath + "/" // Ensure it ends with / for future loads if needed
		fullPath = filepath.Join(baseDir, sectionPath)
	}

	if isDir {
		if err := os.MkdirAll(fullPath, 0755); err != nil {
			return fmt.Errorf("creating directory %s: %w", fullPath, err)
		}

		// Collect current IDs for pruning
		currentIDs := make(map[string]bool, len(items))

		// Write each item to its own file
		for _, item := range items {
			id := getID(item)
			if id == "" {
				continue
			}
			currentIDs[id] = true
			data, err := json.MarshalIndent(item, "", "  ")
			if err != nil {
				return err
			}
			itemPath := filepath.Join(fullPath, id+".json")
			if err := atomicWriteFile(itemPath, data, 0644); err != nil {
				return fmt.Errorf("writing item %s to %s: %w", id, itemPath, err)
			}
		}

		// FIX (2026-08-08): Prune orphaned files. If an item was unregistered,
		// its file remains in the split-layout directory. Clean it up so it
		// doesn't resurface on the next reload/restart. This also handles the
		// case where len(items) == 0 (formerly an early return bug).
		files, err := os.ReadDir(fullPath)
		if err == nil {
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
					continue
				}
				id := strings.TrimSuffix(f.Name(), ".json")
				if !currentIDs[id] {
					_ = os.Remove(filepath.Join(fullPath, f.Name()))
				}
			}
		}
	} else {
		// Write all items to single file
		data, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return err
		}
		if err := atomicWriteFile(fullPath, data, 0644); err != nil {
			return fmt.Errorf("writing section to %s: %w", fullPath, err)
		}
	}
	return nil
}
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".cfgtmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil {
		os.Remove(tmpPath)
		return werr
	}
	if cerr != nil {
		os.Remove(tmpPath)
		return cerr
	}
	_ = os.Chmod(tmpPath, perm)
	if err := os.Rename(tmpPath, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			if remErr := os.Remove(path); remErr == nil {
				if err2 := os.Rename(tmpPath, path); err2 == nil {
					return nil
				}
			}
		}
		os.Remove(tmpPath)
		return fmt.Errorf("atomic write %s: %w", path, err)
	}
	return nil
}
