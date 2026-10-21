//go:build !web && !dev && !desktop

package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// TestServeSSE_NoOsExitOnBindError is a regression test for audit finding C4:
// serveSSE previously called log.Fatalf on ListenAndServe failure, which
// invokes os.Exit(1) and skips all deferred cleanup (CloseMCPConnections,
// JITSessions.CloseAll, etc.). After the fix, it logs the error and triggers
// graceful shutdown via stop(), allowing the function to return normally.
//
// We verify this by occupying a port, then calling serveSSE with that port.
// If the bug is present, the test process is killed by os.Exit(1) and the
// test fails with "process exited". If the fix is in place, serveSSE returns
// within a few seconds and the test passes.
func TestServeSSE_NoOsExitOnBindError(t *testing.T) {
	// Occupy a port so ListenAndServe fails with "address already in use".
	// Bind to ":0" (0.0.0.0) to match serveSSE's binding address — on Windows,
	// 127.0.0.1:port and 0.0.0.0:port are distinct and don't conflict.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to occupy port: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	t.Setenv("VORTEX_PORT", strconv.Itoa(port))

	mcpServer := server.NewMCPServer("test", "1.0.0")
	_, rootCancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		serveSSE(mcpServer, rootCancel) // should return, not os.Exit
	}()

	select {
	case <-done:
		// Success: serveSSE returned normally instead of calling os.Exit.
	case <-time.After(5 * time.Second):
		t.Fatal("serveSSE did not return within 5s — likely stuck or called os.Exit")
	}
}

// TestIsLoopbackAddr pins the P4.1 guard (docs/plan/CORE_BOUNDARY_DEBT_AND_SAAS_GAP_REMEDIATION.md):
// the unauthenticated-bind warning must fire for wildcard/external binds and
// stay silent for loopback binds. A regression here would silently expose the
// raw transport (false "loopback") or spam local dev (false "non-loopback").
func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8000", true},
		{"localhost:8000", true},
		{"[::1]:8000", true},
		{"0.0.0.0:8000", false},
		{"[::]:8000", false},
		{"192.168.1.10:8000", false},
		{"8000", false}, // bare port — no host, i.e. wildcard
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
