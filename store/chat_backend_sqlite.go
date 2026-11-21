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
	`, sessionID, rootID, activeLeafID, createdAt.Format(time.RFC3339))
	return err
}

func (b *SQLiteChatBackend) SaveMessage(ctx context.Context, msg *ChatMessageRow) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO chat_messages (id, session_id, parent_id, client_id, reply_to, forked_from, role, content, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			parent_id=excluded.parent_id,
			client_id=excluded.client_id,
			reply_to=excluded.reply_to,
			forked_from=excluded.forked_from,
			role=excluded.role,
			content=excluded.content
	`, msg.ID, msg.SessionID, msg.ParentID, msg.ClientID, msg.ReplyTo, msg.ForkedFrom, msg.Role, msg.Content, msg.CreatedAt.Format(time.RFC3339))
	return err
}

func (b *SQLiteChatBackend) LoadSession(ctx context.Context, sessionID string) (rootID, activeLeafID string, messages map[string]*ChatMessageRow, err error) {
	var root, leaf sql.NullString
	var createdAtStr string
	err = b.db.QueryRowContext(ctx, `SELECT root_id, active_leaf_id, created_at FROM chat_sessions WHERE id = ?`, sessionID).Scan(&root, &leaf, &createdAtStr)
	if err != nil {
		return "", "", nil, err
	}
	rootID = root.String
	activeLeafID = leaf.String

	rows, err := b.db.QueryContext(ctx, `SELECT id, parent_id, client_id, reply_to, forked_from, role, content, created_at FROM chat_messages WHERE session_id = ?`, sessionID)
	if err != nil {
		return rootID, activeLeafID, nil, err
	}
	defer rows.Close()

	messages = make(map[string]*ChatMessageRow)
	for rows.Next() {
		var m ChatMessageRow
		var parentID, clientID, replyTo, forkedFrom sql.NullString
		var createdStr string
		if err := rows.Scan(&m.ID, &parentID, &clientID, &replyTo, &forkedFrom, &m.Role, &m.Content, &createdStr); err != nil {
			continue
		}
		m.ParentID = parentID.String
		m.ClientID = clientID.String
		m.ReplyTo = replyTo.String
		m.ForkedFrom = forkedFrom.String
		m.SessionID = sessionID
		m.CreatedAt = parseTimeField(createdStr)
		messages[m.ID] = &m
	}
	return rootID, activeLeafID, messages, nil
}

func (b *SQLiteChatBackend) ListSessions(ctx context.Context, limit, offset int) ([]ChatSessionRow, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, root_id, active_leaf_id, created_at FROM chat_sessions ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []ChatSessionRow
	for rows.Next() {
		var s ChatSessionRow
		var root, leaf sql.NullString
		var createdStr string
		if err := rows.Scan(&s.ID, &root, &leaf, &createdStr); err != nil {
			continue
		}
		s.RootID = root.String
		s.ActiveLeafID = leaf.String
		s.CreatedAt = parseTimeField(createdStr)
		sessions = append(sessions, s)
	}
	return sessions, nil
}

func parseTimeField(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", s); err == nil {
		return t
	}
	return time.Time{}
}
