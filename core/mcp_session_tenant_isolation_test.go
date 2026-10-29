package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestEnsureRemoteMCPSession_PerTenantIsolation is the regression test for
// OAUTH §8.2 R4 (2026-10-27): different OwnerID values must produce
// different Mcp-Session-Id handles. Before the fix, the session cache was
// keyed by mcp.ID only, so tenant A's session was silently reused for
// tenant B — letting the remote server conflate two tenants into one session.
//
// Acceptance criterion O4 from the OAUTH plan:
// "两个不同 OwnerID 握手 ⇒ 2 次 initialize、2 个不同 Mcp-Session-Id"
func TestEnsureRemoteMCPSession_PerTenantIsolation(t *testing.T) {
	var initCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["method"] == "initialize" {
			n := atomic.AddInt32(&initCount, 1)
			// Return a unique session ID per initialize call.
			w.Header().Set("Mcp-Session-Id", "sess-tenant-"+string(rune('A'+n-1)))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "multi-tenant-mcp", URL: srv.URL}
	m := NewMCPConnectionManager(nil, nil)

	// Tenant A establishes a session.
	sidA1 := m.ensureRemoteMCPSession(context.Background(), mcp, "tenant-A")
	if sidA1 != "sess-tenant-A" {
		t.Fatalf("tenant-A first call: expected sess-tenant-A, got %q", sidA1)
	}

	// Tenant B establishes a separate session.
	sidB1 := m.ensureRemoteMCPSession(context.Background(), mcp, "tenant-B")
	if sidB1 != "sess-tenant-B" {
		t.Fatalf("tenant-B first call: expected sess-tenant-B, got %q", sidB1)
	}

	// Two different tenants must have different session IDs.
	if sidA1 == sidB1 {
		t.Fatalf("per-tenant isolation violated: tenant-A and tenant-B got the same session %q", sidA1)
	}

	// Exactly 2 initialize calls — one per tenant.
	if got := atomic.LoadInt32(&initCount); got != 2 {
		t.Fatalf("expected 2 initialize calls (one per tenant), got %d", got)
	}

	// Tenant A reuses cached session — no new handshake.
	sidA2 := m.ensureRemoteMCPSession(context.Background(), mcp, "tenant-A")
	if sidA2 != sidA1 {
		t.Fatalf("tenant-A second call: expected cached %q, got %q", sidA1, sidA2)
	}

	// Tenant B reuses cached session — no new handshake.
	sidB2 := m.ensureRemoteMCPSession(context.Background(), mcp, "tenant-B")
	if sidB2 != sidB1 {
		t.Fatalf("tenant-B second call: expected cached %q, got %q", sidB1, sidB2)
	}

	// Still only 2 initialize calls — caching works per-tenant.
	if got := atomic.LoadInt32(&initCount); got != 2 {
		t.Fatalf("expected still 2 initialize calls after cache hits, got %d", got)
	}
}

// TestMcpSessionCacheKey_BackwardCompatible verifies that empty ownerID
// (single-user / operator-credential mode) produces the same key as the
// pre-multi-tenant behavior — just mcpID. This ensures existing deployments
// don't lose their cached sessions on upgrade.
func TestMcpSessionCacheKey_BackwardCompatible(t *testing.T) {
	// Empty ownerID → key is just mcpID (backward compatible).
	if got := mcpSessionCacheKey("mcp-1", ""); got != "mcp-1" {
		t.Fatalf("empty ownerID: expected %q, got %q", "mcp-1", got)
	}

	// Non-empty ownerID → key includes tenant.
	if got := mcpSessionCacheKey("mcp-1", "tenant-A"); got != "mcp-1\x00tenant-A" {
		t.Fatalf("tenant-A: expected %q, got %q", "mcp-1\x00tenant-A", got)
	}

	// Different tenants → different keys.
	keyA := mcpSessionCacheKey("mcp-1", "tenant-A")
	keyB := mcpSessionCacheKey("mcp-1", "tenant-B")
	if keyA == keyB {
		t.Fatalf("different tenants must have different keys: both got %q", keyA)
	}

	// Same tenant → same key.
	keyA2 := mcpSessionCacheKey("mcp-1", "tenant-A")
	if keyA != keyA2 {
		t.Fatalf("same tenant must have same key: got %q and %q", keyA, keyA2)
	}
}
