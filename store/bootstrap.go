package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/config"
)

// BootstrapConfigSync performs the "File-First" startup sync.
//
// Rules:
// 1. Scan workspace/roles/*.json and config.json for role definitions.
// 2. If JSON files exist → overwrite DB with file contents (File-First).
// 3. If JSON files are missing → fall back to DB (LoadRolesFromDB).
// 4. Return the merged role set for Hub runtime initialization.
func BootstrapConfigSync(repo ConfigRepository, rolesDir string) (map[string]*config.Role, error) {
	diskRoles, err := loadRolesFromDisk(rolesDir)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: failed to load roles from disk: %w", err)
	}

	if len(diskRoles) > 0 {
		// File-First: JSON files exist → sync to DB (overwrite)
		if err := repo.SyncFileToDB(diskRoles); err != nil {
			return nil, fmt.Errorf("bootstrap: failed to sync file→db: %w", err)
		}
		return diskRoles, nil
	}

	// No JSON files found → fall back to DB
	dbRoles, err := repo.LoadRolesFromDB()
	if err != nil {
		return nil, fmt.Errorf("bootstrap: failed to load roles from db: %w", err)
	}
	if len(dbRoles) == 0 {
		// Both file and DB are empty → first run, return empty map
		return make(map[string]*config.Role), nil
	}
	return dbRoles, nil
}

// loadRolesFromDisk scans the roles directory for *.json files and parses them.
func loadRolesFromDisk(rolesDir string) (map[string]*config.Role, error) {
	roles := make(map[string]*config.Role)

	matches, err := filepath.Glob(filepath.Join(rolesDir, "*.json"))
	if err != nil {
		return nil, err
	}

	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err != nil {
			continue // skip unreadable files
		}

		var role config.Role
		if err := json.Unmarshal(data, &role); err != nil {
			continue // skip malformed files
		}
		if role.ID == "" {
			// Derive ID from filename if not set
			base := filepath.Base(match)
			role.ID = strings.TrimSuffix(base, filepath.Ext(base))
		}
		roles[role.ID] = &role
	}

	return roles, nil
}

// RoleJSONPath returns the conventional JSON file path for a given role ID.
func RoleJSONPath(rolesDir, roleID string) string {
	return filepath.Join(rolesDir, roleID+".json")
}
