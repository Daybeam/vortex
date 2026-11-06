package oauthclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"golang.org/x/sync/singleflight"
)

// TokenStore provides optional DB persistence for OAuth tokens. When set on
// a TokenManager, tokens survive restarts and are isolated per owner.
// Implemented by store.OAuthTokenStore.
type TokenStore interface {
	SaveToken(ctx context.Context, ownerID, clientID, tokenEndpoint, accessToken, refreshToken string, expiresAt time.Time) error
	LoadToken(ctx context.Context, ownerID, clientID, tokenEndpoint string) (accessToken, refreshToken string, expiresAt time.Time, found bool, err error)
	DeleteAllForOwner(ctx context.Context, ownerID string) error
}

// TokenManager caches and refreshes OAuth 2.0 access tokens for outbound
// connections (orchestrator → external service). It is the client-side
// counterpart to the inbound Authorization Server in pkg/oauth/server.go.
//
// Tokens are cached per (ownerID, clientID, tokenEndpoint) and refreshed
// automatically when they expire (with a 60s safety buffer). Concurrent
// refresh requests for the same key are deduplicated via singleflight.
// When a TokenStore is set, tokens are also persisted to DB so they survive
// restarts and can be loaded on cache miss.
//
// ownerID="" means operator-level (shared, backward-compatible with the
// previous process-level env var behavior).
type TokenManager struct {
	mu     sync.RWMutex
	tokens map[string]*tokenEntry
	client *http.Client
	store  TokenStore // optional DB persistence; nil = in-memory only
	sf     singleflight.Group
}

type tokenEntry struct {
	accessToken  string
	refreshToken string
	expiresAt    time.Time
}

// NewTokenManager creates a TokenManager with in-memory caching only (no DB).
func NewTokenManager() *TokenManager {
	return &TokenManager{
		tokens: make(map[string]*tokenEntry),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// NewTokenManagerWithStore creates a TokenManager with DB-backed persistence.
// Tokens are saved to the store on fetch/refresh and loaded on cache miss.
func NewTokenManagerWithStore(store TokenStore) *TokenManager {
	return &TokenManager{
		tokens: make(map[string]*tokenEntry),
		client: &http.Client{Timeout: 10 * time.Second},
		store:  store,
	}
}

// GetToken returns a valid access token for the given config and owner.
// ownerID="" means operator-level (shared credential).
//
// Cache lookup order: in-memory → DB (if store set) → token endpoint.
// On successful fetch/refresh, the token is cached in-memory and persisted
// to DB (if store set). Concurrent calls for the same key are deduplicated.
func (tm *TokenManager) GetToken(ctx context.Context, cfg config.OAuthClientConfig, ownerID string) (string, error) {
	key := cacheKey(cfg, ownerID)

	// Fast path: check in-memory cache under read lock.
	tm.mu.RLock()
	entry, ok := tm.tokens[key]
	tm.mu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt.Add(-60*time.Second)) {
		return entry.accessToken, nil
	}

	// Slow path: fetch or refresh. Deduplicate concurrent calls.
	val, err, _ := tm.sf.Do(key, func() (any, error) {
		return tm.fetchOrRefresh(ctx, cfg, ownerID, key)
	})
	if err != nil {
		return "", err
	}
	return val.(string), nil
}

func (tm *TokenManager) fetchOrRefresh(ctx context.Context, cfg config.OAuthClientConfig, ownerID, key string) (string, error) {
	// Check DB on cache miss (if store is set).
	if tm.store != nil {
		accessToken, refreshToken, expiresAt, found, err := tm.store.LoadToken(ctx, ownerID, cfg.ClientID, cfg.TokenEndpoint)
		if err == nil && found && time.Now().Before(expiresAt.Add(-60*time.Second)) {
			// DB has a valid token — cache it and return.
			tm.mu.Lock()
			tm.tokens[key] = &tokenEntry{accessToken: accessToken, refreshToken: refreshToken, expiresAt: expiresAt}
			tm.mu.Unlock()
			return accessToken, nil
		}
	}

	grant := cfg.GrantType
	if grant == "" {
		grant = "client_credentials"
	}

	switch grant {
	case "client_credentials":
		return tm.fetchClientCredentials(ctx, cfg, ownerID, key)
	case "refresh_token":
		return tm.fetchWithRefreshToken(ctx, cfg, ownerID, key)
	default:
		return "", fmt.Errorf("oauth: unsupported grant_type %q (use client_credentials or refresh_token)", grant)
	}
}

func (tm *TokenManager) fetchClientCredentials(ctx context.Context, cfg config.OAuthClientConfig, ownerID, key string) (string, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
	}
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}

	resp, err := tm.postTokenRequest(ctx, cfg.TokenEndpoint, form)
	if err != nil {
		return "", fmt.Errorf("oauth client_credentials: %w", err)
	}

	expiresAt := time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	tm.mu.Lock()
	tm.tokens[key] = &tokenEntry{accessToken: resp.AccessToken, expiresAt: expiresAt}
	tm.mu.Unlock()
	tm.persist(ctx, ownerID, cfg.ClientID, cfg.TokenEndpoint, resp.AccessToken, "", expiresAt)
	return resp.AccessToken, nil
}

func (tm *TokenManager) fetchWithRefreshToken(ctx context.Context, cfg config.OAuthClientConfig, ownerID, key string) (string, error) {
	// Get the refresh token: from in-memory cache, DB, or env var.
	refreshToken := tm.lookupRefreshToken(ctx, cfg, ownerID, key)
	if refreshToken == "" {
		return "", fmt.Errorf("oauth refresh_token: no refresh token available (env %q is empty and no cached token)", cfg.RefreshTokenEnv)
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"refresh_token": {refreshToken},
	}

	resp, err := tm.postTokenRequest(ctx, cfg.TokenEndpoint, form)
	if err != nil {
		return "", fmt.Errorf("oauth refresh_token: %w", err)
	}

	newRefresh := resp.RefreshToken
	if newRefresh == "" {
		newRefresh = refreshToken
	}

	expiresAt := time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second)
	tm.mu.Lock()
	tm.tokens[key] = &tokenEntry{accessToken: resp.AccessToken, refreshToken: newRefresh, expiresAt: expiresAt}
	tm.mu.Unlock()
	tm.persist(ctx, ownerID, cfg.ClientID, cfg.TokenEndpoint, resp.AccessToken, newRefresh, expiresAt)
	return resp.AccessToken, nil
}

// lookupRefreshToken finds the refresh token from in-memory cache, DB, or env var.
func (tm *TokenManager) lookupRefreshToken(ctx context.Context, cfg config.OAuthClientConfig, ownerID, key string) string {
	// In-memory cache.
	tm.mu.RLock()
	entry, hasCache := tm.tokens[key]
	tm.mu.RUnlock()
	if hasCache && entry.refreshToken != "" {
		return entry.refreshToken
	}
	// DB.
	if tm.store != nil {
		_, refreshToken, _, found, _ := tm.store.LoadToken(ctx, ownerID, cfg.ClientID, cfg.TokenEndpoint)
		if found && refreshToken != "" {
			return refreshToken
		}
	}
	// Env var (initial bootstrap).
	if cfg.RefreshTokenEnv != "" {
		return os.Getenv(cfg.RefreshTokenEnv)
	}
	return ""
}

// persist saves the token to DB if a store is set. Best-effort: errors are
// not returned to the caller because the in-memory cache is already updated
// and the token is valid for this request. A failed persist means the token
// won't survive a restart — logged but not fatal.
func (tm *TokenManager) persist(ctx context.Context, ownerID, clientID, tokenEndpoint, accessToken, refreshToken string, expiresAt time.Time) {
	if tm.store != nil {
		_ = tm.store.SaveToken(ctx, ownerID, clientID, tokenEndpoint, accessToken, refreshToken, expiresAt)
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func (tm *TokenManager) postTokenRequest(ctx context.Context, endpoint string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := tm.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned empty access_token")
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 3600 // default 1h if not specified
	}
	return &tr, nil
}

// SetStore attaches a TokenStore for DB-backed persistence. Called after
// construction once the DB is available. Tokens fetched after this call will
// be persisted to DB and loaded on cache miss. Existing in-memory tokens are
// unaffected.
func (tm *TokenManager) SetStore(store TokenStore) {
	tm.mu.Lock()
	tm.store = store
	tm.mu.Unlock()
}

// Invalidate clears the cached token for the given config and owner.
func (tm *TokenManager) Invalidate(cfg config.OAuthClientConfig, ownerID string) {
	key := cacheKey(cfg, ownerID)
	tm.mu.Lock()
	delete(tm.tokens, key)
	tm.mu.Unlock()
}

// InvalidateAll clears all in-memory cached tokens.
func (tm *TokenManager) InvalidateAll() {
	tm.mu.Lock()
	tm.tokens = make(map[string]*tokenEntry)
	tm.mu.Unlock()
}

func cacheKey(cfg config.OAuthClientConfig, ownerID string) string {
	return ownerID + "|" + cfg.ClientID + "|" + cfg.TokenEndpoint
}
