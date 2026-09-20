package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/daybeam/vortex/config"
)

type SQLiteRepo struct {
	db *sql.DB
}

func NewSQLiteRepo(db *sql.DB) *SQLiteRepo {
	return &SQLiteRepo{db: db}
}

// SaveRoleAndPersistFile implements the "File-First" write policy:
// 1. Transactional Update DB
// 2. Write-back to JSON file
func (r *SQLiteRepo) SaveRoleAndPersistFile(role *config.Role, targetPath string) error {
	// 1. Update DB Transactionally
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	raw, err := json.Marshal(role)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`
		INSERT INTO roles_meta (id, name, base_capability, provider, model, raw_json)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			base_capability=excluded.base_capability,
			provider=excluded.provider,
			model=excluded.model,
			raw_json=excluded.raw_json,
			updated_at=CURRENT_TIMESTAMP
	`, role.ID, role.Name, role.BaseCapability, role.Provider, role.Model, raw)
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// 2. Persist to JSON file
	fileData, err := json.MarshalIndent(role, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal role to json: %w", err)
	}

	return os.WriteFile(targetPath, fileData, 0644)
}

func (r *SQLiteRepo) SyncFileToDB(roles map[string]*config.Role) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, role := range roles {
		raw, _ := json.Marshal(role)
		_, err := tx.Exec(`
			INSERT OR REPLACE INTO roles_meta (id, name, base_capability, provider, model, raw_json)
			VALUES (?, ?, ?, ?, ?, ?)
		`, role.ID, role.Name, role.BaseCapability, role.Provider, role.Model, raw)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadRolesFromDB reads all roles from the DB.
// Used as a fallback when JSON files are missing.
func (r *SQLiteRepo) LoadRolesFromDB() (map[string]*config.Role, error) {
	rows, err := r.db.QueryContext(context.Background(), `SELECT raw_json FROM roles_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	roles := make(map[string]*config.Role)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var role config.Role
		if err := json.Unmarshal([]byte(raw), &role); err != nil {
			return nil, fmt.Errorf("failed to unmarshal role from db: %w", err)
		}
		roles[role.ID] = &role
	}
	return roles, nil
}

// DeleteRole removes a role from the DB and optionally deletes its JSON file.
func (r *SQLiteRepo) DeleteRole(roleID, jsonFilePath string) error {
	// 1. Delete from DB
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`DELETE FROM roles_meta WHERE id = ?`, roleID)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// 2. Optionally delete JSON file
	if jsonFilePath != "" {
		if _, err := os.Stat(jsonFilePath); err == nil {
			return os.Remove(jsonFilePath)
		}
		// File doesn't exist — that's fine
	}
	return nil
}
