package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
)

// Tests for resolveMCPURL and callRemoteMCPTool, added 2026-07-11 alongside
// the URL-based remote MCP execution path (previously only discovery
// existed; every actual tool call to a url-based MCP failed with "cloud
// tool execution not supported locally yet"). Covers both the header-based
// auth mechanism (pre-existing, AuthHeaderName/APIKeyEnv) and the new
// URL-embedded-token mechanism (URLAuthPlaceholder), motivated by Tushare's
// official hosted MCP using a URL of the form
// "https://api.tushare.pro/mcp/token=<token>" rather than a header.

func TestResolveMCPURL_NoPlaceholder_ReturnsURLUnchanged(t *testing.T) {
	mcp := &config.MCPDef{ID: "exa", URL: "https://mcp.exa.ai/mcp"}
	got, err := resolveMCPURL(mcp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != mcp.URL {
		t.Fatalf("expected URL unchanged, got %q", got)
	}
}

func TestResolveMCPURL_NoURL_Errors(t *testing.T) {
	mcp := &config.MCPDef{ID: "empty"}
	_, err := resolveMCPURL(mcp)
	if err == nil {
		t.Fatal("expected error for empty URL, got nil")
	}
}

func TestResolveMCPURL_PlaceholderSubstitution_TushareStyle(t *testing.T) {
	t.Setenv("TEST_TUSHARE_TOKEN_XYZ", "abc123secret")
	mcp := &config.MCPDef{
		ID:                 "tushare",
		URL:                "https://api.tushare.pro/mcp/token={{TOKEN}}",
		URLAuthPlaceholder: "{{TOKEN}}",
		APIKeyEnv:          "TEST_TUSHARE_TOKEN_XYZ",
	}
	got, err := resolveMCPURL(mcp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "https://api.tushare.pro/mcp/token=abc123secret"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveMCPURL_PlaceholderSet_NoAPIKeyEnv_Errors(t *testing.T) {
	mcp := &config.MCPDef{
		ID:                 "tushare",
		URL:                "https://api.tushare.pro/mcp/token={{TOKEN}}",
		URLAuthPlaceholder: "{{TOKEN}}",
	}
	_, err := resolveMCPURL(mcp)
	if err == nil {
		t.Fatal("expected error when url_auth_placeholder is set but api_key_env is not, got nil")
	}
}

func TestResolveMCPURL_PlaceholderSet_EnvUnset_Errors(t *testing.T) {
	mcp := &config.MCPDef{
		ID:                 "tushare",
		URL:                "https://api.tushare.pro/mcp/token={{TOKEN}}",
		URLAuthPlaceholder: "{{TOKEN}}",
		APIKeyEnv:          "TEST_DEFINITELY_UNSET_VAR_ABC123",
	}
	_ = os.Unsetenv("TEST_DEFINITELY_UNSET_VAR_ABC123")
	_, err := resolveMCPURL(mcp)
	if err == nil {
		t.Fatal("expected error when the referenced env var is unset, got nil")
	}
}

func TestResolveMCPURL_PlaceholderNotInURL_Errors(t *testing.T) {
	t.Setenv("TEST_TOKEN_ABC", "secret")
	mcp := &config.MCPDef{
		ID:                 "misconfigured",
		URL:                "https://example.com/mcp",
		URLAuthPlaceholder: "{{NOT_PRESENT}}",
		APIKeyEnv:          "TEST_TOKEN_ABC",
	}
	_, err := resolveMCPURL(mcp)
	if err == nil {
		t.Fatal("expected error when placeholder string is not found in URL, got nil")
	}
}

func TestCallRemoteMCPTool_SuccessWithHeaderAuth(t *testing.T) {
	var gotAuthHeader string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("x-api-key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer srv.Close()

	t.Setenv("TEST_EXA_KEY", "sekret")
	mcp := &config.MCPDef{
		ID:             "exa",
		URL:            srv.URL,
		AuthHeaderName: "x-api-key",
		APIKeyEnv:      "TEST_EXA_KEY",
	}

	m := NewMCPConnectionManager(nil, nil)
	result, err := m.callRemoteMCPTool(context.Background(), mcp, "search", map[string]any{"query": "foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuthHeader != "sekret" {
		t.Fatalf("expected auth header %q, got %q", "sekret", gotAuthHeader)
	}
	if gotBody["method"] != "tools/call" {
		t.Fatalf("expected method tools/call, got %v", gotBody["method"])
	}
	params, _ := gotBody["params"].(map[string]any)
	if params["name"] != "search" {
		t.Fatalf("expected tool name 'search' in request params, got %v", params["name"])
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestCallRemoteMCPTool_SuccessWithURLPlaceholderAuth(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer srv.Close()

	t.Setenv("TEST_TUSHARE_KEY", "tok999")
	mcp := &config.MCPDef{
		ID:                 "tushare",
		URL:                srv.URL + "/mcp/token={{TOKEN}}",
		URLAuthPlaceholder: "{{TOKEN}}",
		APIKeyEnv:          "TEST_TUSHARE_KEY",
	}

	m := NewMCPConnectionManager(nil, nil)
	_, err := m.callRemoteMCPTool(context.Background(), mcp, "query", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantPath := "/mcp/token=tok999"
	if gotPath != wantPath {
		t.Fatalf("expected request path %q (token substituted), got %q", wantPath, gotPath)
	}
}

func TestCallRemoteMCPTool_JSONRPCError_Propagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"tool not found"}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "x", URL: srv.URL}
	m := NewMCPConnectionManager(nil, nil)
	_, err := m.callRemoteMCPTool(context.Background(), mcp, "nonexistent", map[string]any{})
	if err == nil {
		t.Fatal("expected error for JSON-RPC error response, got nil")
	}
}

func TestCallRemoteMCPTool_HTTPError_Propagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "x", URL: srv.URL}
	m := NewMCPConnectionManager(nil, nil)
	_, err := m.callRemoteMCPTool(context.Background(), mcp, "whatever", map[string]any{})
	if err == nil {
		t.Fatal("expected error for HTTP 500 response, got nil")
	}
}
