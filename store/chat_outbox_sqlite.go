package store

import (
	"context"
	"database/sql"
	"time"
)

// maxOutboxAttempts bounds how many times one row may be handed to a
// deliverer. Without it a transport that always fails -- a revoked bot token,
// a chat that was deleted -- rotates through the queue forever and the backlog
// never closes.
const maxOutboxAttempts = 5

// defaultOutboxLease is how long a claim is held when the caller passes no
// lease. It only needs to outlast one send, which is a single HTTP round trip.
const defaultOutboxLease = 30 * time.Second

// SQLiteChatOutbox implements IChatOutbox on the runtime DB.
type SQLiteChatOutbox struct {
	db *sql.DB
}

func NewSQLiteChatOutbox(db *sql.DB) *SQLiteChatOutbox {
	return &SQLiteChatOutbox{db: db}
}

var _ IChatOutbox = (*SQLiteChatOutbox)(nil)

func (o *SQLiteChatOutbox) Enqueue(ctx context.Context, item OutboxItem) (bool, error) {
	res, err := o.db.ExecContext(ctx, `
		INSERT INTO chat_outbox (session_id, kind, dedupe_key, text, meta_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(dedupe_key) DO NOTHING
	`, item.SessionID, item.Kind, item.DedupeKey, item.Text, item.MetaJSON, time.Now().UTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// claimablePredicate is the one definition of "somebody may take this row":
// either it was never taken, or its holder's lease lapsed without confirmation
// and it has attempts left. The list query and the guarded update both splice
// it in, so a claim can never rest on a row the list was not allowed to name.
const claimablePredicate = `(status = 'pending' OR (status = 'claimed' AND claim_expires_at < ?)) AND attempts < ?`

func (o *SQLiteChatOutbox) ClaimPending(ctx context.Context, holder string, limit int, lease time.Duration) ([]OutboxItem, error) {
	if limit <= 0 {
		limit = 1
	}
	if lease <= 0 {
		lease = defaultOutboxLease
	}
	now := time.Now().Unix()

	// A row whose attempts ran out while its holder died mid-send is otherwise
	// stuck non-terminal and unclaimable forever; close it here so Purge can
	// eventually remove it.
	if _, err := o.db.ExecContext(ctx, `
		UPDATE chat_outbox SET status = 'dropped', last_error = 'attempts exhausted'
		WHERE attempts >= ? AND (status = 'pending' OR (status = 'claimed' AND claim_expires_at < ?))
	`, maxOutboxAttempts, now); err != nil {
		return nil, err
	}

	// Audit FIX 2026-11-22: replaced 1+2N query pattern (SELECT ids → N×UPDATE →
	// N×SELECT) with a single UPDATE...RETURNING. The inner SELECT picks claimable
	// IDs; the outer predicate re-checks claimability for atomicity (if another hub
	// claims a row between SELECT and UPDATE, the outer predicate won't match and
	// RETURNING won't emit that row). SQLite 3.35+ supports RETURNING; modernc/sqlite
	// v1.29.0 embeds 3.45+.
	rows, err := o.db.QueryContext(ctx, `
		UPDATE chat_outbox
		SET status = 'claimed', claim_holder = ?, claim_expires_at = ?, attempts = attempts + 1
		WHERE id IN (
			SELECT id FROM chat_outbox
			WHERE `+claimablePredicate+`
			ORDER BY id LIMIT ?
		)
		AND `+claimablePredicate+`
		RETURNING id, session_id, kind, dedupe_key, text, meta_json, created_at, attempts
	`, holder, now+int64(lease.Seconds()), now, maxOutboxAttempts, limit,
		now, maxOutboxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OutboxItem
	for rows.Next() {
		var item OutboxItem
		var createdAt sql.NullTime
		if err := rows.Scan(&item.ID, &item.SessionID, &item.Kind, &item.DedupeKey,
			&item.Text, &item.MetaJSON, &createdAt, &item.Attempts); err != nil {
			return out, err
		}
		item.CreatedAt = createdAt.Time
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// loadHeld re-reads a row the caller just won. Matching on claim_holder is a
// fence, not a routine: it only bites if this hub's lease lapsed between the
// winning UPDATE and this read, and the effect is an error rather than handing
// out a row another hub is already sending.
func (o *SQLiteChatOutbox) loadHeld(ctx context.Context, id int64, holder string) (OutboxItem, error) {
	var item OutboxItem
	var createdAt sql.NullTime
	err := o.db.QueryRowContext(ctx, `
		SELECT id, session_id, kind, dedupe_key, text, meta_json, created_at, attempts
		FROM chat_outbox WHERE id = ? AND claim_holder = ?
	`, id, holder).Scan(&item.ID, &item.SessionID, &item.Kind, &item.DedupeKey,
		&item.Text, &item.MetaJSON, &createdAt, &item.Attempts)
	if err != nil {
		return OutboxItem{}, err
	}
	item.CreatedAt = createdAt.Time
	return item, nil
}

func (o *SQLiteChatOutbox) MarkDelivered(ctx context.Context, id int64, holder string) (bool, error) {
	res, err := o.db.ExecContext(ctx, `
		UPDATE chat_outbox SET status = 'delivered', delivered_at = ?, claim_expires_at = 0, last_error = ''
		WHERE id = ? AND status = 'claimed' AND claim_holder = ?
	`, time.Now().UTC(), id, holder)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (o *SQLiteChatOutbox) MarkFailed(ctx context.Context, id int64, holder, reason string) error {
	// attempts was already raised by the claim, so a row at the cap goes
	// straight to 'dropped' instead of re-arming.
	_, err := o.db.ExecContext(ctx, `
		UPDATE chat_outbox
		SET status = CASE WHEN attempts >= ? THEN 'dropped' ELSE 'pending' END,
		    claim_holder = '', claim_expires_at = 0, last_error = ?
		WHERE id = ? AND status = 'claimed' AND claim_holder = ?
	`, maxOutboxAttempts, reason, id, holder)
	return err
}

func (o *SQLiteChatOutbox) Purge(ctx context.Context, before time.Time) (int, error) {
	res, err := o.db.ExecContext(ctx, `
		DELETE FROM chat_outbox WHERE status IN ('delivered', 'dropped') AND created_at < ?
	`, before.UTC())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (o *SQLiteChatOutbox) RowExists(ctx context.Context, dedupeKey string) (bool, error) {
	var id int64
	err := o.db.QueryRowContext(ctx, `SELECT id FROM chat_outbox WHERE dedupe_key = ? LIMIT 1`, dedupeKey).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
