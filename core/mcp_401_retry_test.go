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

// ── R3: 401 ⇒ invalidate session, retry once (OAUTH §4 L2) ──────────────────
//
// A remote MCP returning 401 means the Mcp-Session-Id is stale. The engine
// must invalidate the cached session for this (mcp, tenant) pair and retry
// exactly once. A second 401 is returned as an error — no infinite loop.

func TestCallRemoteMCPTool_401_RetriesOnceAndSucceeds(t *testing.T) {
	// Server returns 401 on the first tools/call, 200 on the second.
	var toolCallCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		isToolCall := body["method"] == "tools/call"
		if isToolCall {
			n := toolCallCount.Add(1)
			if n == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"session expired"}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "test-401", URL: srv.URL}
	m := NewMCPConnectionManager(nil, nil)

	result, err := m.callRemoteMCPTool(context.Background(), mcp, "search", map[string]any{}, "")
	if err != nil {
		t.Fatalf("expected success after 401 retry, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result after retry")
	}
	if got := toolCallCount.Load(); got != 2 {
		t.Fatalf("expected exactly 2 tool calls (1 initial + 1 retry), got %d", got)
	}
}

func TestCallRemoteMCPTool_401_TwiceReturnsError(t *testing.T) {
	// Server always returns 401. The engine must retry once, then give up.
	var toolCallCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count only tools/call requests, not initialize handshakes.
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["method"] == "tools/call" {
			toolCallCount.Add(1)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "test-401-always", URL: srv.URL}
	m := NewMCPConnectionManager(nil, nil)

	_, err := m.callRemoteMCPTool(context.Background(), mcp, "search", map[string]any{}, "")
	if err == nil {
		t.Fatal("expected error after two consecutive 401s, got nil")
	}
	if got := toolCallCount.Load(); got != 2 {
		t.Fatalf("expected exactly 2 tool calls (1 initial + 1 retry), got %d — "+
			"the engine must not loop infinitely on 401", got)
	}
}

func TestCallRemoteMCPTool_401_RetryInvalidatesSessionCache(t *testing.T) {
	// After a 401 retry, the stale session cache entry must be gone so the
	// next call starts fresh (not reusing the dead session).
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"session expired"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "test-401-invalidate", URL: srv.URL}
	m := NewMCPConnectionManager(nil, nil)

	// Pre-populate the session cache with a stale entry.
	cacheKey := mcpSessionCacheKey(mcp.ID, "")
	m.mu.Lock()
	m.remoteSessionCache = map[string]string{cacheKey: "stale-session-id"}
	m.mu.Unlock()

	_, err := m.callRemoteMCPTool(context.Background(), mcp, "search", map[string]any{}, "")
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}

	// The stale entry must have been invalidated during the 401 retry.
	// (The retry may have cached a new session, but the old "stale-session-id"
	// must not be the value.)
	m.mu.Lock()
	defer m.mu.Unlock()
	if sid := m.remoteSessionCache[cacheKey]; sid == "stale-session-id" {
		t.Fatal("stale session ID must not remain in cache after 401 retry")
	}
}

// ── R7: hot-reload clears session cache (OAUTH §4 L2) ───────────────────────

func TestInvalidateSessionCache_ClearsAllEntries(t *testing.T) {
	m := NewMCPConnectionManager(nil, nil)

	// Populate the session cache with multiple entries.
	m.mu.Lock()
	m.remoteSessionCache = map[string]string{
		"mcp1\x00tenantA": "session-1A",
		"mcp1\x00tenantB": "session-1B",
		"mcp2":            "session-2",
	}
	m.mu.Unlock()

	m.InvalidateSessionCache()

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.remoteSessionCache) != 0 {
		t.Fatalf("expected empty session cache after InvalidateSessionCache, got %d entries: %v",
			len(m.remoteSessionCache), m.remoteSessionCache)
	}
}

func TestInvalidateSessionCacheEntry_RemovesOnlyOneEntry(t *testing.T) {
	m := NewMCPConnectionManager(nil, nil)

	m.mu.Lock()
	m.remoteSessionCache = map[string]string{
		"mcp1\x00tenantA": "session-1A",
		"mcp1\x00tenantB": "session-1B",
		"mcp2":            "session-2",
	}
	m.mu.Unlock()

	// Invalidate only the (mcp1, tenantA) entry — per-tenant failure isolation (R3/O8).
	m.invalidateSessionCacheEntry("mcp1", "tenantA")

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.remoteSessionCache) != 2 {
		t.Fatalf("expected 2 remaining entries after single invalidation, got %d: %v",
			len(m.remoteSessionCache), m.remoteSessionCache)
	}
	if _, ok := m.remoteSessionCache["mcp1\x00tenantA"]; ok {
		t.Fatal("(mcp1, tenantA) entry should have been removed")
	}
	if _, ok := m.remoteSessionCache["mcp1\x00tenantB"]; !ok {
		t.Fatal("(mcp1, tenantB) entry should still be present — per-tenant isolation")
	}
}

// Verify that InvalidateSessionCache is safe to call on a nil-initialized cache.
func TestInvalidateSessionCache_NilSafe(t *testing.T) {
	m := NewMCPConnectionManager(nil, nil)
	// Should not panic on a fresh manager with nil/empty cache.
	m.InvalidateSessionCache()
}
