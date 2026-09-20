package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/daybeam/vortex/config"
)

// Tests for ensureRemoteMCPSession / performRemoteMCPHandshake, added
// 2026-07-18 alongside the MCP Streamable HTTP session-ID support. Motivated
// by https://huggingface.co/mcp returning a JSON-RPC error ("Session ID
// required") on tools/list without a prior initialize handshake -- see the
// 2026-07-17 addendum for the discovery trail. exa/tushare have never
// required this and must remain unaffected; TestCallRemoteMCPTool_* in
// remote_mcp_test.go continuing to pass is the regression guard for that.

// TestEnsureRemoteMCPSession_NilManagerLiteral_NoPanic is a direct
// regression test for a real bug caught by the existing test suite while
// developing this feature: ensureRemoteMCPSession originally wrote straight
// into the cache map without a nil check, which panicked
// ("assignment to entry in nil map") for any *MCPConnectionManager constructed as a bare
// struct literal rather than via NewMCPConnectionManager.
func TestEnsureRemoteMCPSession_NilManagerLiteral_NoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "nil-cache-test", URL: srv.URL}
	m := &MCPConnectionManager{} // deliberately bypass NewMCPConnectionManager

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ensureRemoteMCPSession panicked on a MCPConnectionManager with a nil remoteSessionCache: %v", r)
		}
	}()
	_ = m.ensureRemoteMCPSession(context.Background(), mcp)
}

func TestEnsureRemoteMCPSession_CapturesHeaderAndAppliesToSubsequentCall(t *testing.T) {
	var initCount, listCount int32
	var sawSessionIDOnList string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		method, _ := body["method"].(string)

		switch method {
		case "initialize":
			atomic.AddInt32(&initCount, 1)
			w.Header().Set("Mcp-Session-Id", "sess-abc-123")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26"}}`))
		case "notifications/initialized":
			// one-way notification, no id in the request; just acknowledge
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			atomic.AddInt32(&listCount, 1)
			sawSessionIDOnList = r.Header.Get("Mcp-Session-Id")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"ping","description":"d"}]}}`))
		default:
			t.Errorf("unexpected method in request: %q", method)
		}
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "hf-style", URL: srv.URL}
	m := NewMCPConnectionManager(nil, nil)

	sid := m.ensureRemoteMCPSession(context.Background(), mcp)
	if sid != "sess-abc-123" {
		t.Fatalf("expected captured session id %q, got %q", "sess-abc-123", sid)
	}
	if atomic.LoadInt32(&initCount) != 1 {
		t.Fatalf("expected exactly 1 initialize call, got %d", initCount)
	}

	tools, err := m.discoverRemoteMCPTools(context.Background(), mcp)
	if err != nil {
		t.Fatalf("unexpected error from discoverRemoteMCPTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("unexpected tools result: %+v", tools)
	}
	if sawSessionIDOnList != "sess-abc-123" {
		t.Fatalf("expected tools/list request to carry Mcp-Session-Id %q, got %q", "sess-abc-123", sawSessionIDOnList)
	}
}

func TestEnsureRemoteMCPSession_NoSessionHeader_ReturnsEmptyAndDoesNotBreakExistingFlow(t *testing.T) {
	// Mirrors exa/tushare's real behavior: never returns Mcp-Session-Id at
	// all. Confirms this feature is a strict no-op for servers that don't
	// use the session mechanism.
	var sawSessionHeaderOnList bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		method, _ := body["method"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "initialize":
			// No Mcp-Session-Id header set -- session not required.
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{}}`))
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "" {
				sawSessionHeaderOnList = true
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
		}
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "exa-style", URL: srv.URL}
	m := NewMCPConnectionManager(nil, nil)

	sid := m.ensureRemoteMCPSession(context.Background(), mcp)
	if sid != "" {
		t.Fatalf("expected empty session id when server never returns Mcp-Session-Id, got %q", sid)
	}

	if _, err := m.discoverRemoteMCPTools(context.Background(), mcp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sawSessionHeaderOnList {
		t.Fatal("Mcp-Session-Id header should not have been set on tools/list when no session was established")
	}
}

func TestEnsureRemoteMCPSession_CachesPerMCPID(t *testing.T) {
	var initCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["method"] == "initialize" {
			atomic.AddInt32(&initCount, 1)
			w.Header().Set("Mcp-Session-Id", "sess-cached")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "cache-test", URL: srv.URL}
	m := NewMCPConnectionManager(nil, nil)

	first := m.ensureRemoteMCPSession(context.Background(), mcp)
	second := m.ensureRemoteMCPSession(context.Background(), mcp)

	if first != "sess-cached" || second != "sess-cached" {
		t.Fatalf("expected both calls to return %q, got %q and %q", "sess-cached", first, second)
	}
	if atomic.LoadInt32(&initCount) != 1 {
		t.Fatalf("expected exactly 1 initialize call across two ensureRemoteMCPSession calls (caching should prevent a second), got %d", initCount)
	}
}

func TestEnsureRemoteMCPSession_HandshakeFails_ReturnsEmptyGracefully(t *testing.T) {
	// No server at all -- connection refused. Confirms a network failure
	// during the handshake degrades to "no session" rather than propagating
	// an error or panicking, matching this feature's documented contract.
	mcp := &config.MCPDef{ID: "unreachable", URL: "http://127.0.0.1:1"}
	m := NewMCPConnectionManager(nil, nil)

	sid := m.ensureRemoteMCPSession(context.Background(), mcp)
	if sid != "" {
		t.Fatalf("expected empty session id on handshake failure, got %q", sid)
	}
}

// TestEnsureRemoteMCPSession_ConcurrentAccess_NoDuplicateHandshake is a
// regression test for audit H4 (TOCTOU race): before the per-MCP-ID lock,
// N concurrent callers could all miss the cache and perform N separate
// HTTP handshakes, leaking N-1 sessions on the remote server.
func TestEnsureRemoteMCPSession_ConcurrentAccess_NoDuplicateHandshake(t *testing.T) {
	var initCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["method"] == "initialize" {
			atomic.AddInt32(&initCount, 1)
			w.Header().Set("Mcp-Session-Id", "sess-concurrent")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{}}`))
	}))
	defer srv.Close()

	mcp := &config.MCPDef{ID: "concurrent-test", URL: srv.URL}
	m := NewMCPConnectionManager(nil, nil)

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			sid := m.ensureRemoteMCPSession(context.Background(), mcp)
			if sid != "sess-concurrent" {
				t.Errorf("expected session %q, got %q", "sess-concurrent", sid)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&initCount); got != 1 {
		t.Fatalf("expected exactly 1 initialize call from %d concurrent goroutines, got %d (audit H4 TOCTOU race)", N, got)
	}
}
