package store

import (
	"context"
	"database/sql"
	"time"
)

type SQLiteChatBackend struct {
	db *sql.DB
}

func NewSQLiteChatBackend(db *sql.DB) *SQLiteChatBackend {
	return &SQLiteChatBackend{db: db}
}

func (b *SQLiteChatBackend) SaveSession(ctx context.Context, sessionID, rootID, activeLeafID string, createdAt time.Time) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO chat_sessions (id, root_id, active_leaf_id, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			root_id=excluded.root_id,
			active_leaf_id=excluded.active_leaf_id
	`, sessionID, rootID, activeLeafID, createdAt)
	return err
}

func (b *SQLiteChatBackend) SaveMessage(ctx context.Context, msgID, sessionID, parentID, role, content string, createdAt time.Time) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO chat_messages (id, session_id, parent_id, role, content, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			parent_id=excluded.parent_id,
			role=excluded.role,
			content=excluded.content
	`, msgID, sessionID, parentID, role, content, createdAt)
	return err
}

func (b *SQLiteChatBackend) LoadSession(ctx context.Context, sessionID string) (rootID, activeLeafID string, messages map[string]*ChatMessageRow, err error) {
	var root, leaf sql.NullString
	var createdAt time.Time
	err = b.db.QueryRowContext(ctx, `SELECT root_id, active_leaf_id, created_at FROM chat_sessions WHERE id = ?`, sessionID).Scan(&root, &leaf, &createdAt)
	if err != nil {
		return "", "", nil, err
	}
	rootID = root.String
	activeLeafID = leaf.String

	rows, err := b.db.QueryContext(ctx, `SELECT id, parent_id, role, content, created_at FROM chat_messages WHERE session_id = ?`, sessionID)
	if err != nil {
		return rootID, activeLeafID, nil, err
	}
	defer rows.Close()

	messages = make(map[string]*ChatMessageRow)
	for rows.Next() {
		var m ChatMessageRow
		var parentID sql.NullString
		if err := rows.Scan(&m.ID, &parentID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			continue
		}
		m.ParentID = parentID.String
		m.SessionID = sessionID
		messages[m.ID] = &m
	}
	return rootID, activeLeafID, messages, nil
}

func (b *SQLiteChatBackend) ListSessions(ctx context.Context, limit int) ([]ChatSessionRow, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, root_id, active_leaf_id, created_at FROM chat_sessions ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []ChatSessionRow
	for rows.Next() {
		var s ChatSessionRow
		var root, leaf sql.NullString
		if err := rows.Scan(&s.ID, &root, &leaf, &s.CreatedAt); err != nil {
			continue
		}
		s.RootID = root.String
		s.ActiveLeafID = leaf.String
		sessions = append(sessions, s)
	}
	return sessions, nil
}
