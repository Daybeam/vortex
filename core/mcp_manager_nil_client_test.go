package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/config"
)

// Regression tests for H4: nil httpClient fallback in
// MCPConnectionManager.performRemoteMCPHandshake.
//
// Before the fix, performRemoteMCPHandshake used m.httpClient directly. If a
// MCPConnectionManager was constructed as a bare struct literal (bypassing
// NewMCPConnectionManager), httpClient would be nil and the HTTP request would
// panic with a nil pointer dereference. The fix adds:
//   httpClient := m.httpClient
//   if httpClient == nil {
//       httpClient = http.DefaultClient
//   }
//
// The existing TestEnsureRemoteMCPSession_NilManagerLiteral_NoPanic in
// remote_mcp_session_test.go covers the ensureRemoteMCPSession path. These
// tests cover performRemoteMCPHandshake directly and verify the actual HTTP
// call succeeds (not just that it doesn't panic).

// TestPerformRemoteMCPHandshake_NilHTTPClient_UsesDefault verifies that a
// bare MCPConnectionManager literal (nil httpClient) can still perform the
// MCP handshake via http.DefaultClient, returning the session ID from the
// response header.
func TestPerformRemoteMCPHandshake_NilHTTPClient_UsesDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session-from-default-client")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26"}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "nil-http-test", URL: srv.URL}
	// Deliberately bypass NewMCPConnectionManager — httpClient is nil.
	m := &MCPConnectionManager{}

	sid := m.performRemoteMCPHandshake(context.Background(), mcp)
	if sid != "test-session-from-default-client" {
		t.Fatalf("expected session ID from default client handshake, got %q (nil httpClient fallback not working)", sid)
	}
}

// TestPerformRemoteMCPHandshake_NilHTTPClient_NoPanicOnHandshakeError
// verifies that even when the server returns an error response, the nil
// httpClient fallback doesn't panic — it just returns an empty session ID.
func TestPerformRemoteMCPHandshake_NilHTTPClient_NoPanicOnHandshakeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"error":{"code":-32603,"message":"internal"}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "nil-http-err-test", URL: srv.URL}
	m := &MCPConnectionManager{}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("performRemoteMCPHandshake panicked with nil httpClient on error response: %v", r)
		}
	}()
	sid := m.performRemoteMCPHandshake(context.Background(), mcp)
	// No session ID header on a 500 response — should return empty string.
	if sid != "" {
		t.Fatalf("expected empty session ID on 500 response, got %q", sid)
	}
}
