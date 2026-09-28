package providers

import (
	"context"
	"net"
	"strings"
	"testing"
)

// TestSSRFSafeTransport_BlocksPrivateIPs is a regression test for audit NEW-3:
// ssrfSafeTransport must block connections to private/loopback/link-local IPs
// at connection time (DialContext), preventing the DNS rebinding TOCTOU race
// that was possible when validation happened only at URL parse time.
func TestSSRFSafeTransport_BlocksPrivateIPs(t *testing.T) {
	transport := ssrfSafeTransport()

	blockedAddrs := []string{
		"10.0.0.1:80",          // private class A
		"172.16.0.1:80",        // private class B
		"192.168.1.1:80",       // private class C
		"169.254.169.254:80",   // link-local (AWS metadata)
		"0.0.0.0:80",           // unspecified
	}

	for _, addr := range blockedAddrs {
		conn, err := transport.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			if conn != nil {
				conn.Close()
			}
			t.Errorf("addr %q: expected block error, but connection succeeded", addr)
		}
	}
}

// TestSSRFSafeTransport_AllowsLocalhost verifies that localhost connections
// are still permitted (needed for local dev and testing).
func TestSSRFSafeTransport_AllowsLocalhost(t *testing.T) {
	transport := ssrfSafeTransport()

	// We can't actually connect (no server), but we can verify that the
	// dial proceeds past the SSRF guard without a "blocked IP" error.
	// The connection will fail with "connection refused", not "SSRF guard".
	_, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if err != nil && strings.Contains(err.Error(), "SSRF guard") {
		t.Errorf("localhost should not be blocked by SSRF guard, got: %v", err)
	}
}

// TestSSRFSafeTransport_FailsClosedOnDNSFailure is a regression test for
// audit NEW-6: when DNS lookup fails, the transport must return an error
// (fail-closed), not allow the request through to the HTTP client.
func TestSSRFSafeTransport_FailsClosedOnDNSFailure(t *testing.T) {
	// First check if .invalid actually fails DNS on this system.
	// Some ISPs/proxies resolve all domains (captive portal), making this
	// test environment-dependent. Skip if DNS doesn't actually fail.
	_, lookupErr := net.DefaultResolver.LookupIPAddr(context.Background(), "nonexistent-host-xyz.invalid")
	if lookupErr == nil {
		t.Skip("DNS resolver returns results for .invalid TLD (ISP redirect); cannot test fail-closed on this system")
	}

	transport := ssrfSafeTransport()
	_, err := transport.DialContext(context.Background(), "tcp", "nonexistent-host-xyz.invalid:80")
	if err == nil {
		t.Fatal("expected DNS failure error for .invalid domain, got nil")
	}
	if !strings.Contains(err.Error(), "SSRF guard") {
		t.Errorf("expected error to contain 'SSRF guard', got: %v", err)
	}
}
