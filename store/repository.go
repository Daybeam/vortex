package store

import (
	"github.com/daybeam/vortex/config"
)

// ConfigRepository is the storage abstraction for configuration data.
// Implementations may back onto SQLite, BoltDB, Postgres, etc.
// Business logic should ONLY depend on this interface, never on concrete impls.
type ConfigRepository interface {
	// SaveRoleAndPersistFile writes a role to the DB transactionally,
	// then writes back to the corresponding JSON file (File-First policy).
	SaveRoleAndPersistFile(role *config.Role, targetPath string) error

	// SyncFileToDB bulk-upserts all roles from JSON files into the DB.
	// Called during startup bootstrap to sync file → DB.
	SyncFileToDB(roles map[string]*config.Role) error

	// LoadRolesFromDB reads all roles from the DB (used when JSON files
	// are missing or as a fallback read path).
	LoadRolesFromDB() (map[string]*config.Role, error)

	// DeleteRole removes a role from both DB and (optionally) its JSON file.
	DeleteRole(roleID, jsonFilePath string) error
}
