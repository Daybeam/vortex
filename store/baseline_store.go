package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// SQLiteBaselineStore implements BaselineStore over SQLite.
type SQLiteBaselineStore struct {
	db *sql.DB
}

func NewBaselineStore(db *sql.DB) *SQLiteBaselineStore {
	return &SQLiteBaselineStore{db: db}
}

func (s *SQLiteBaselineStore) PinBaseline(ctx context.Context, taskID, sopID, sopVer, roleID, roleVer string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO task_baselines
		(task_id, sop_id, sop_version, role_id, role_version, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, taskID, sopID, sopVer, roleID, roleVer)
	return err
}

func (s *SQLiteBaselineStore) GetBaseline(ctx context.Context, taskID string) (*TaskBaseline, error) {
	var b TaskBaseline
	err := s.db.QueryRowContext(ctx, `
		SELECT task_id, sop_id, sop_version, role_id, role_version
		FROM task_baselines WHERE task_id = ?
	`, taskID).Scan(&b.TaskID, &b.SOPID, &b.SOPVersion, &b.RoleID, &b.RoleVersion)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *SQLiteBaselineStore) LoadPinnedSOP(ctx context.Context, sopID, sopVer string) (*schemas.SOP, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `
		SELECT raw_json FROM sop_versions WHERE id = ? AND version = ?
	`, sopID, sopVer).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("sop %q version %q not found in archive", sopID, sopVer)
	}
	if err != nil {
		return nil, err
	}
	var sop schemas.SOP
	if err := json.Unmarshal([]byte(raw), &sop); err != nil {
		return nil, fmt.Errorf("unmarshal pinned sop: %w", err)
	}
	return &sop, nil
}

func (s *SQLiteBaselineStore) LoadPinnedRole(ctx context.Context, roleID, roleVer string) (*config.Role, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `
		SELECT raw_json FROM role_versions WHERE id = ? AND version = ?
	`, roleID, roleVer).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("role %q version %q not found in archive", roleID, roleVer)
	}
	if err != nil {
		return nil, err
	}
	var role config.Role
	if err := json.Unmarshal([]byte(raw), &role); err != nil {
		return nil, fmt.Errorf("unmarshal pinned role: %w", err)
	}
	return &role, nil
}
