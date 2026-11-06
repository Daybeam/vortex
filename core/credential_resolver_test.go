package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/oauthclient"
)

func TestCredentialResolver_OAuth_ResolveBearerToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "resolver-test-token",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(ts.Close)

	tm := oauthclient.NewTokenManager()
	resolver := NewOAuthCredentialResolver(tm)

	mcp := &config.MCPDef{
		ID: "test-oauth-mcp",
		OAuthClient: &config.OAuthClientConfig{
			ClientID:      "test-client",
			ClientSecret:  "secret",
			TokenEndpoint: ts.URL,
		},
	}

	cred, err := resolver.Resolve(context.Background(), mcp, "user-1")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if cred.HeaderName != "Authorization" {
		t.Errorf("expected Authorization, got %s", cred.HeaderName)
	}
	if cred.HeaderValue != "Bearer resolver-test-token" {
		t.Errorf("expected Bearer resolver-test-token, got %s", cred.HeaderValue)
	}
}

func TestCredentialResolver_APIKey_ResolveFromEnv(t *testing.T) {
	os.Setenv("TEST_CRED_API_KEY", "my-api-secret")
	t.Cleanup(func() { os.Unsetenv("TEST_CRED_API_KEY") })

	resolver := NewOAuthCredentialResolver(nil) // nil TM — API key still works

	mcp := &config.MCPDef{
		ID:              "test-apikey-mcp",
		AuthHeaderName:  "X-API-Key",
		AuthHeaderPrefix: "Bearer ",
		APIKeyEnv:       "TEST_CRED_API_KEY",
	}

	cred, err := resolver.Resolve(context.Background(), mcp, "")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if cred.HeaderName != "X-API-Key" {
		t.Errorf("expected X-API-Key, got %s", cred.HeaderName)
	}
	if cred.HeaderValue != "Bearer my-api-secret" {
		t.Errorf("expected 'Bearer my-api-secret', got %s", cred.HeaderValue)
	}
}

func TestCredentialResolver_NoAuth_ReturnsEmpty(t *testing.T) {
	resolver := NewOAuthCredentialResolver(nil)

	mcp := &config.MCPDef{ID: "no-auth-mcp"}
	cred, err := resolver.Resolve(context.Background(), mcp, "")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if cred.HeaderName != "" {
		t.Errorf("expected empty credential, got %+v", cred)
	}
}

func TestCredentialResolver_OAuthNilTM_ReturnsError(t *testing.T) {
	resolver := NewOAuthCredentialResolver(nil)

	mcp := &config.MCPDef{
		ID: "oauth-no-tm",
		OAuthClient: &config.OAuthClientConfig{
			ClientID:      "c",
			ClientSecret:  "s",
			TokenEndpoint: "http://localhost:1",
		},
	}

	_, err := resolver.Resolve(context.Background(), mcp, "")
	if err == nil {
		t.Error("expected error for OAuth with nil TM, got nil")
	}
}

func TestCredentialResolver_APIKeyMissingEnv_ReturnsError(t *testing.T) {
	resolver := NewOAuthCredentialResolver(nil)

	mcp := &config.MCPDef{
		ID:         "apikey-missing-env",
		AuthHeaderName: "X-API-Key",
		APIKeyEnv:  "NONEXISTENT_CRED_ENV_99999",
	}

	_, err := resolver.Resolve(context.Background(), mcp, "")
	if err == nil {
		t.Error("expected error for missing env var, got nil")
	}
}

func TestCredentialResolver_Invalidate_ClearsToken(t *testing.T) {
	var counter atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := counter.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-" + strconv.Itoa(int(n)),
			"expires_in":   3600,
		})
	}))
	t.Cleanup(ts.Close)

	tm := oauthclient.NewTokenManager()
	resolver := NewOAuthCredentialResolver(tm)

	mcp := &config.MCPDef{
		ID: "invalidate-test-mcp",
		OAuthClient: &config.OAuthClientConfig{
			ClientID:      "inv-client",
			ClientSecret:  "secret",
			TokenEndpoint: ts.URL,
		},
	}

	// Fetch token.
	cred1, err := resolver.Resolve(context.Background(), mcp, "user-1")
	if err != nil {
		t.Fatalf("first Resolve failed: %v", err)
	}

	// Cached — second call should not hit endpoint.
	cred2, err := resolver.Resolve(context.Background(), mcp, "user-1")
	if err != nil {
		t.Fatalf("second Resolve failed: %v", err)
	}
	if cred1.HeaderValue != cred2.HeaderValue {
		t.Errorf("expected cached token, got different values")
	}
	if counter.Load() != 1 {
		t.Errorf("expected 1 endpoint call, got %d", counter.Load())
	}

	// Invalidate — next call should hit endpoint again.
	resolver.Invalidate(mcp.ID, "user-1")
	_, err = resolver.Resolve(context.Background(), mcp, "user-1")
	if err != nil {
		t.Fatalf("Resolve after invalidate failed: %v", err)
	}
	if counter.Load() != 2 {
		t.Errorf("expected 2 endpoint calls after invalidate, got %d", counter.Load())
	}
}

func TestCredentialResolver_PerUserIsolation(t *testing.T) {
	var counter atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := counter.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "user-token-" + strconv.Itoa(int(n)),
			"expires_in":   3600,
		})
	}))
	t.Cleanup(ts.Close)

	tm := oauthclient.NewTokenManager()
	resolver := NewOAuthCredentialResolver(tm)

	mcp := &config.MCPDef{
		ID: "per-user-mcp",
		OAuthClient: &config.OAuthClientConfig{
			ClientID:      "shared-client",
			ClientSecret:  "secret",
			TokenEndpoint: ts.URL,
		},
	}

	credA, err := resolver.Resolve(context.Background(), mcp, "user-A")
	if err != nil {
		t.Fatalf("Resolve for user-A failed: %v", err)
	}
	credB, err := resolver.Resolve(context.Background(), mcp, "user-B")
	if err != nil {
		t.Fatalf("Resolve for user-B failed: %v", err)
	}
	if credA.HeaderValue == credB.HeaderValue {
		t.Errorf("expected different tokens for different users, both got %s", credA.HeaderValue)
	}
}

func TestCredentialResolver_Invalidate_APIKeyOnly_NoOp(t *testing.T) {
	// Invalidate on an API-key-only MCP should be a no-op (no panic, no error).
	resolver := NewOAuthCredentialResolver(nil)
	resolver.Invalidate("apikey-only-mcp", "user-1") // should not panic
}

func TestCredential_Apply_SetsHeader(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	cred := Credential{HeaderName: "Authorization", HeaderValue: "Bearer abc123"}
	cred.Apply(req)

	if got := req.Header.Get("Authorization"); got != "Bearer abc123" {
		t.Errorf("expected Bearer abc123, got %s", got)
	}
}

func TestCredential_Apply_EmptyIsNoOp(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	cred := Credential{}
	cred.Apply(req)

	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("expected empty header, got %s", got)
	}
}
