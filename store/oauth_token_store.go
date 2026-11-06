package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// OAuthTokenStore persists per-user OAuth tokens to SQLite. Used by
// pkg/oauthclient.TokenManager to survive restarts and isolate credentials
// per user (OAUTH_SHARED_MCP_MULTI_TENANT_DESIGN.md §4 L2, R5).
//
// Methods use primitive types to satisfy the oauthclient.TokenStore interface
// without creating a package dependency (pkg/oauthclient does not import store).
type OAuthTokenStore struct {
	db *sql.DB
}

func NewOAuthTokenStore(db *sql.DB) *OAuthTokenStore {
	return &OAuthTokenStore{db: db}
}

// SaveToken upserts a token entry. Called by TokenManager after each
// successful token fetch or refresh — R5 requires "先落盘再返回" (persist
// before returning) to avoid concurrent refresh burning the refresh token.
func (s *OAuthTokenStore) SaveToken(ctx context.Context, ownerID, clientID, tokenEndpoint, accessToken, refreshToken string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO oauth_tokens (owner_id, client_id, token_endpoint, access_token, refresh_token, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner_id, client_id, token_endpoint) DO UPDATE SET
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			expires_at=excluded.expires_at,
			updated_at=CURRENT_TIMESTAMP
	`, ownerID, clientID, tokenEndpoint, accessToken, refreshToken, expiresAt)
	return err
}

// LoadToken reads a token entry. found=false means no row — the caller
// treats this as "must fetch from token endpoint".
func (s *OAuthTokenStore) LoadToken(ctx context.Context, ownerID, clientID, tokenEndpoint string) (accessToken, refreshToken string, expiresAt time.Time, found bool, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT access_token, refresh_token, expires_at
		FROM oauth_tokens
		WHERE owner_id = ? AND client_id = ? AND token_endpoint = ?
	`, ownerID, clientID, tokenEndpoint)

	var rt sql.NullString
	err = row.Scan(&accessToken, &rt, &expiresAt)
	if err == sql.ErrNoRows {
		return "", "", time.Time{}, false, nil
	}
	if err != nil {
		return "", "", time.Time{}, false, fmt.Errorf("oauth token store: query: %w", err)
	}
	refreshToken = rt.String
	return accessToken, refreshToken, expiresAt, true, nil
}

// DeleteAllForOwner removes all tokens for a given owner. Called on config
// hot-reload (R7) to force re-fetch with potentially updated credentials.
// ownerID="" clears operator-level tokens.
func (s *OAuthTokenStore) DeleteAllForOwner(ctx context.Context, ownerID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE owner_id = ?`, ownerID)
	return err
}
