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

func (b *SQLiteChatBackend) SaveSession(ctx context.Context, sessionID, rootID, activeLeafID, ownerID string, shared bool, createdAt time.Time) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO chat_sessions (id, root_id, active_leaf_id, owner_id, shared, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			root_id=excluded.root_id,
			active_leaf_id=excluded.active_leaf_id,
			owner_id=CASE WHEN chat_sessions.owner_id='' THEN excluded.owner_id ELSE chat_sessions.owner_id END,
			shared=excluded.shared
	`, sessionID, rootID, activeLeafID, ownerID, shared, createdAt.Format(time.RFC3339))
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

func (b *SQLiteChatBackend) LoadSession(ctx context.Context, sessionID string) (rootID, activeLeafID, ownerID string, shared bool, messages map[string]*ChatMessageRow, err error) {
	var root, leaf, owner sql.NullString
	var createdAtStr string
	err = b.db.QueryRowContext(ctx, `SELECT root_id, active_leaf_id, owner_id, shared, created_at FROM chat_sessions WHERE id = ?`, sessionID).Scan(&root, &leaf, &owner, &shared, &createdAtStr)
	if err != nil {
		return "", "", "", false, nil, err
	}
	rootID = root.String
	activeLeafID = leaf.String
	ownerID = owner.String

	rows, err := b.db.QueryContext(ctx, `SELECT id, parent_id, client_id, reply_to, forked_from, role, content, created_at FROM chat_messages WHERE session_id = ?`, sessionID)
	if err != nil {
		return rootID, activeLeafID, ownerID, shared, nil, err
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
	return rootID, activeLeafID, ownerID, shared, messages, rows.Err()
}

func (b *SQLiteChatBackend) ListSessions(ctx context.Context, scope SessionScope, limit, offset int) ([]ChatSessionRow, error) {
	query := `SELECT id, root_id, active_leaf_id, owner_id, shared, created_at FROM chat_sessions`
	args := []any{}
	if !scope.All {
		query += ` WHERE owner_id = ?`
		args = append(args, scope.Owner)
	}
	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []ChatSessionRow
	for rows.Next() {
		var s ChatSessionRow
		var root, leaf, owner sql.NullString
		var createdStr string
		if err := rows.Scan(&s.ID, &root, &leaf, &owner, &s.Shared, &createdStr); err != nil {
			continue
		}
		s.RootID = root.String
		s.ActiveLeafID = leaf.String
		s.OwnerID = owner.String
		s.CreatedAt = parseTimeField(createdStr)
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func (b *SQLiteChatBackend) SavePendingReply(ctx context.Context, row PendingReplyRow) error {
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO chat_pending_replies (session_id, message_id, content, started_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			message_id = excluded.message_id,
			content = excluded.content,
			started_at = excluded.started_at
	`, row.SessionID, row.MessageID, row.Content, row.StartedAt.Format(time.RFC3339))
	return err
}

func (b *SQLiteChatBackend) ClearPendingReply(ctx context.Context, sessionID string) error {
	_, err := b.db.ExecContext(ctx, `DELETE FROM chat_pending_replies WHERE session_id = ?`, sessionID)
	return err
}

func (b *SQLiteChatBackend) ListPendingReplies(ctx context.Context) ([]PendingReplyRow, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT session_id, message_id, content, started_at FROM chat_pending_replies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pending []PendingReplyRow
	for rows.Next() {
		var p PendingReplyRow
		var startedStr string
		if err := rows.Scan(&p.SessionID, &p.MessageID, &p.Content, &startedStr); err != nil {
			continue
		}
		p.StartedAt = parseTimeField(startedStr)
		pending = append(pending, p)
	}
	return pending, nil
}

// parseTimeField parses a stored created_at/started_at value back into
// time.Time. Tries RFC3339 first (new format); falls back to the legacy
// time.String() format for rows written before the fix. Returns zero
// time.Time on unparseable input (zero-value defense).
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
