package oauthclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

// tokenEndpointTestServer creates a test HTTP server that acts as an OAuth
// token endpoint. It counts requests and returns configurable responses.
type tokenEndpointTestServer struct {
	*httptest.Server
	requestCount atomic.Int32
}

func newTokenEndpointTestServer(t *testing.T, status int, resp any) *tokenEndpointTestServer {
	t.Helper()
	ts := &tokenEndpointTestServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestTokenManager_ClientCredentials_FetchAndCache(t *testing.T) {
	ts := newTokenEndpointTestServer(t, 200, map[string]any{
		"access_token": "test-token-123",
		"expires_in":   3600,
		"token_type":   "Bearer",
	})

	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:      "test-client",
		ClientSecret:  "test-secret",
		TokenEndpoint: ts.URL,
		GrantType:     "client_credentials",
	}

	// First call: should hit the endpoint.
	token, err := tm.GetToken(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("GetToken failed: %v", err)
	}
	if token != "test-token-123" {
		t.Errorf("expected test-token-123, got %s", token)
	}
	if ts.requestCount.Load() != 1 {
		t.Errorf("expected 1 endpoint call, got %d", ts.requestCount.Load())
	}

	// Second call: should use cache (no new endpoint call).
	token2, err := tm.GetToken(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("second GetToken failed: %v", err)
	}
	if token2 != "test-token-123" {
		t.Errorf("expected cached test-token-123, got %s", token2)
	}
	if ts.requestCount.Load() != 1 {
		t.Errorf("expected 1 endpoint call (cached), got %d", ts.requestCount.Load())
	}
}

func TestTokenManager_ClientCredentials_DefaultGrantType(t *testing.T) {
	ts := newTokenEndpointTestServer(t, 200, map[string]any{
		"access_token": "default-grant-token",
		"expires_in":   3600,
	})

	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:      "test-client",
		ClientSecret:  "test-secret",
		TokenEndpoint: ts.URL,
		// GrantType intentionally empty — should default to client_credentials.
	}

	token, err := tm.GetToken(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("GetToken with empty grant_type failed: %v", err)
	}
	if token != "default-grant-token" {
		t.Errorf("expected default-grant-token, got %s", token)
	}
}

func TestTokenManager_RefreshToken_FromEnv(t *testing.T) {
	os.Setenv("TEST_OAUTH_REFRESH_TOKEN", "initial-refresh-token")
	t.Cleanup(func() { os.Unsetenv("TEST_OAUTH_REFRESH_TOKEN") })

	ts := newTokenEndpointTestServer(t, 200, map[string]any{
		"access_token":  "refreshed-access-token",
		"refresh_token": "new-refresh-token",
		"expires_in":    3600,
	})

	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:        "test-client",
		ClientSecret:    "test-secret",
		TokenEndpoint:   ts.URL,
		GrantType:       "refresh_token",
		RefreshTokenEnv: "TEST_OAUTH_REFRESH_TOKEN",
	}

	token, err := tm.GetToken(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("GetToken failed: %v", err)
	}
	if token != "refreshed-access-token" {
		t.Errorf("expected refreshed-access-token, got %s", token)
	}
}

func TestTokenManager_RefreshToken_MissingEnv(t *testing.T) {
	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:        "test-client",
		ClientSecret:    "test-secret",
		TokenEndpoint:   "http://localhost:1",
		GrantType:       "refresh_token",
		RefreshTokenEnv: "NONEXISTENT_ENV_VAR_12345",
	}

	_, err := tm.GetToken(context.Background(), cfg, "")
	if err == nil {
		t.Error("expected error for missing refresh token env, got nil")
	}
	if !strings.Contains(err.Error(), "no refresh token") {
		t.Errorf("expected 'no refresh token' error, got: %v", err)
	}
}

func TestTokenManager_ErrorOnNon200(t *testing.T) {
	ts := newTokenEndpointTestServer(t, 401, map[string]any{
		"error":             "invalid_client",
		"error_description": "Unknown client",
	})

	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:      "bad-client",
		ClientSecret:  "bad-secret",
		TokenEndpoint: ts.URL,
	}

	_, err := tm.GetToken(context.Background(), cfg, "")
	if err == nil {
		t.Error("expected error for 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error, got: %v", err)
	}
}

func TestTokenManager_ErrorOnEmptyAccessToken(t *testing.T) {
	ts := newTokenEndpointTestServer(t, 200, map[string]any{
		"access_token": "",
		"expires_in":   3600,
	})

	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:      "test-client",
		ClientSecret:  "test-secret",
		TokenEndpoint: ts.URL,
	}

	_, err := tm.GetToken(context.Background(), cfg, "")
	if err == nil {
		t.Error("expected error for empty access_token, got nil")
	}
}

func TestTokenManager_UnsupportedGrantType(t *testing.T) {
	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:      "test-client",
		ClientSecret:  "test-secret",
		TokenEndpoint: "http://localhost:1",
		GrantType:     "password",
	}

	_, err := tm.GetToken(context.Background(), cfg, "")
	if err == nil {
		t.Error("expected error for unsupported grant_type, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported grant_type") {
		t.Errorf("expected unsupported grant_type error, got: %v", err)
	}
}

func TestTokenManager_InvalidateClearsCache(t *testing.T) {
	ts := newTokenEndpointTestServer(t, 200, map[string]any{
		"access_token": "token-before-invalidate",
		"expires_in":   3600,
	})

	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:      "test-client",
		ClientSecret:  "test-secret",
		TokenEndpoint: ts.URL,
	}

	// Fetch and cache.
	_, err := tm.GetToken(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("first GetToken failed: %v", err)
	}
	if ts.requestCount.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", ts.requestCount.Load())
	}

	// Invalidate.
	tm.Invalidate(cfg, "")

	// Next call should hit the endpoint again.
	_, err = tm.GetToken(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("GetToken after invalidate failed: %v", err)
	}
	if ts.requestCount.Load() != 2 {
		t.Errorf("expected 2 calls after invalidate, got %d", ts.requestCount.Load())
	}
}

func TestTokenManager_InvalidateAllClearsAll(t *testing.T) {
	ts := newTokenEndpointTestServer(t, 200, map[string]any{
		"access_token": "token-all",
		"expires_in":   3600,
	})

	tm := NewTokenManager()
	cfg1 := config.OAuthClientConfig{ClientID: "c1", ClientSecret: "s", TokenEndpoint: ts.URL}
	cfg2 := config.OAuthClientConfig{ClientID: "c2", ClientSecret: "s", TokenEndpoint: ts.URL}

	_, _ = tm.GetToken(context.Background(), cfg1, "")
	_, _ = tm.GetToken(context.Background(), cfg2, "")
	if ts.requestCount.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", ts.requestCount.Load())
	}

	tm.InvalidateAll()

	_, _ = tm.GetToken(context.Background(), cfg1, "")
	_, _ = tm.GetToken(context.Background(), cfg2, "")
	if ts.requestCount.Load() != 4 {
		t.Errorf("expected 4 calls after InvalidateAll, got %d", ts.requestCount.Load())
	}
}

func TestTokenManager_ScopesSentInRequest(t *testing.T) {
	var receivedForm string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedForm = r.FormValue("scope")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "scoped-token",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(ts.Close)

	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:      "test-client",
		ClientSecret:  "test-secret",
		TokenEndpoint: ts.URL,
		Scopes:        []string{"read", "write", "admin"},
	}

	_, err := tm.GetToken(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("GetToken failed: %v", err)
	}
	if receivedForm != "read write admin" {
		t.Errorf("expected scopes 'read write admin', got %q", receivedForm)
	}
}

func TestTokenManager_DefaultExpiryWhenMissing(t *testing.T) {
	ts := newTokenEndpointTestServer(t, 200, map[string]any{
		"access_token": "no-expiry-token",
		// expires_in intentionally omitted
	})

	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:      "test-client",
		ClientSecret:  "test-secret",
		TokenEndpoint: ts.URL,
	}

	token, err := tm.GetToken(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("GetToken failed: %v", err)
	}
	if token != "no-expiry-token" {
		t.Errorf("expected no-expiry-token, got %s", token)
	}
	// Should be cached (default 1h expiry). Second call should not hit endpoint.
	_, err = tm.GetToken(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("second GetToken failed: %v", err)
	}
	if ts.requestCount.Load() != 1 {
		t.Errorf("expected 1 call (cached with default expiry), got %d", ts.requestCount.Load())
	}
}

// Ensure TokenManager is safe for concurrent use.
func TestTokenManager_ConcurrentAccess(t *testing.T) {
	ts := newTokenEndpointTestServer(t, 200, map[string]any{
		"access_token": "concurrent-token",
		"expires_in":   3600,
	})

	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:      "concurrent-client",
		ClientSecret:  "secret",
		TokenEndpoint: ts.URL,
	}

	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_, err := tm.GetToken(context.Background(), cfg, "")
			done <- err
		}()
	}
	for i := 0; i < 10; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent GetToken failed: %v", err)
		}
	}
	// singleflight should deduplicate: only 1 endpoint call for 10 concurrent requests.
	if ts.requestCount.Load() != 1 {
		t.Errorf("expected 1 endpoint call (singleflight), got %d", ts.requestCount.Load())
	}
}

// --- Per-user isolation tests ---

// mockTokenStore is an in-memory TokenStore for testing DB persistence paths
// without importing the store package (which would create a circular dep).
type mockTokenStore struct {
	tokens map[string]struct {
		access, refresh string
		expiresAt       time.Time
	}
}

func newMockTokenStore() *mockTokenStore {
	return &mockTokenStore{tokens: make(map[string]struct {
		access, refresh string
		expiresAt       time.Time
	})}
}

func (m *mockTokenStore) SaveToken(_ context.Context, ownerID, clientID, tokenEndpoint, accessToken, refreshToken string, expiresAt time.Time) error {
	key := ownerID + "|" + clientID + "|" + tokenEndpoint
	m.tokens[key] = struct {
		access, refresh string
		expiresAt       time.Time
	}{accessToken, refreshToken, expiresAt}
	return nil
}

func (m *mockTokenStore) LoadToken(_ context.Context, ownerID, clientID, tokenEndpoint string) (string, string, time.Time, bool, error) {
	key := ownerID + "|" + clientID + "|" + tokenEndpoint
	entry, ok := m.tokens[key]
	if !ok {
		return "", "", time.Time{}, false, nil
	}
	return entry.access, entry.refresh, entry.expiresAt, true, nil
}

func (m *mockTokenStore) DeleteAllForOwner(_ context.Context, ownerID string) error {
	for k := range m.tokens {
		if strings.HasPrefix(k, ownerID+"|") {
			delete(m.tokens, k)
		}
	}
	return nil
}

// TestTokenManager_PerUserIsolation verifies that two different ownerIDs get
// different tokens from the same endpoint, and each owner's token is cached
// independently.
func TestTokenManager_PerUserIsolation(t *testing.T) {
	var counter atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := counter.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("token-user-%d", n),
			"expires_in":   3600,
		})
	}))
	t.Cleanup(ts.Close)

	tm := NewTokenManager()
	cfg := config.OAuthClientConfig{
		ClientID:      "shared-client",
		ClientSecret:  "secret",
		TokenEndpoint: ts.URL,
	}

	// User A fetches a token.
	tokenA, err := tm.GetToken(context.Background(), cfg, "user-A")
	if err != nil {
		t.Fatalf("GetToken for user-A failed: %v", err)
	}

	// User B fetches a token — should be different.
	tokenB, err := tm.GetToken(context.Background(), cfg, "user-B")
	if err != nil {
		t.Fatalf("GetToken for user-B failed: %v", err)
	}
	if tokenA == tokenB {
		t.Errorf("expected different tokens for different users, both got %q", tokenA)
	}

	// User A's cached token should still be the same on second call.
	tokenA2, err := tm.GetToken(context.Background(), cfg, "user-A")
	if err != nil {
		t.Fatalf("second GetToken for user-A failed: %v", err)
	}
	if tokenA2 != tokenA {
		t.Errorf("expected cached token %q for user-A, got %q", tokenA, tokenA2)
	}

	// 2 endpoint calls: one per user.
	if counter.Load() != 2 {
		t.Errorf("expected 2 endpoint calls (one per user), got %d", counter.Load())
	}
}

// TestTokenManager_DBPersistence_LoadOnCacheMiss verifies that a token saved
// to the store is loaded when a new TokenManager (e.g. after restart) checks
// the DB on cache miss.
func TestTokenManager_DBPersistence_LoadOnCacheMiss(t *testing.T) {
	store := newMockTokenStore()

	// Phase 1: fetch a token with DB persistence.
	ts := newTokenEndpointTestServer(t, 200, map[string]any{
		"access_token": "persisted-token",
		"expires_in":   3600,
	})

	tm1 := NewTokenManagerWithStore(store)
	cfg := config.OAuthClientConfig{
		ClientID:      "persist-client",
		ClientSecret:  "secret",
		TokenEndpoint: ts.URL,
	}

	token1, err := tm1.GetToken(context.Background(), cfg, "user-X")
	if err != nil {
		t.Fatalf("GetToken failed: %v", err)
	}
	if token1 != "persisted-token" {
		t.Fatalf("expected persisted-token, got %s", token1)
	}

	// Phase 2: new TokenManager with same store — should load from DB, not hit endpoint.
	tm2 := NewTokenManagerWithStore(store)
	token2, err := tm2.GetToken(context.Background(), cfg, "user-X")
	if err != nil {
		t.Fatalf("GetToken from new TM failed: %v", err)
	}
	if token2 != "persisted-token" {
		t.Errorf("expected persisted-token loaded from DB, got %s", token2)
	}

	// Only 1 endpoint call total — the second TM loaded from DB.
	if ts.requestCount.Load() != 1 {
		t.Errorf("expected 1 endpoint call (second loaded from DB), got %d", ts.requestCount.Load())
	}
}

// TestTokenManager_SetStore_AttachesPersistence verifies that SetStore
// upgrades an in-memory-only TokenManager to DB-backed.
func TestTokenManager_SetStore_AttachesPersistence(t *testing.T) {
	store := newMockTokenStore()
	ts := newTokenEndpointTestServer(t, 200, map[string]any{
		"access_token": "set-store-token",
		"expires_in":   3600,
	})

	tm := NewTokenManager() // in-memory only initially
	tm.SetStore(store)      // upgrade to DB-backed

	cfg := config.OAuthClientConfig{
		ClientID:      "set-store-client",
		ClientSecret:  "secret",
		TokenEndpoint: ts.URL,
	}

	_, err := tm.GetToken(context.Background(), cfg, "user-Y")
	if err != nil {
		t.Fatalf("GetToken failed: %v", err)
	}

	// Token should be in the store.
	access, _, _, found, _ := store.LoadToken(context.Background(), "user-Y", cfg.ClientID, cfg.TokenEndpoint)
	if !found {
		t.Error("expected token in store after SetStore + GetToken")
	}
	if access != "set-store-token" {
		t.Errorf("expected set-store-token in store, got %s", access)
	}
}
