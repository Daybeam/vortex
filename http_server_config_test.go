package main

import (
	"net/http"
	"testing"
	"time"
)

// TestNewHardenedHTTPServer_HasTimeouts is the regression test for audit C1:
// HTTP servers in consumer/desktop editions must have non-zero ReadTimeout,
// WriteTimeout, and IdleTimeout to prevent Slowloris DoS and connection
// exhaustion. This test fails if anyone reverts to http.ListenAndServe
// (which uses zero-value timeouts) or removes the timeout configuration.
func TestNewHardenedHTTPServer_HasTimeouts(t *testing.T) {
	srv := newHardenedHTTPServer("127.0.0.1:0", http.NewServeMux())

	if srv.ReadTimeout == 0 {
		t.Error("ReadTimeout is zero — vulnerable to Slowloris DoS (audit C1)")
	}
	if srv.WriteTimeout == 0 {
		t.Error("WriteTimeout is zero — hung connections can persist forever (audit C1)")
	}
	if srv.IdleTimeout == 0 {
		t.Error("IdleTimeout is zero — keep-alive connections never close (audit C1)")
	}

	// Sanity: values should be reasonable (at least 10s each)
	minTimeout := 10 * time.Second
	if srv.ReadTimeout < minTimeout {
		t.Errorf("ReadTimeout %v is too short (minimum %v)", srv.ReadTimeout, minTimeout)
	}
	if srv.WriteTimeout < minTimeout {
		t.Errorf("WriteTimeout %v is too short (minimum %v)", srv.WriteTimeout, minTimeout)
	}
	if srv.IdleTimeout < minTimeout {
		t.Errorf("IdleTimeout %v is too short (minimum %v)", srv.IdleTimeout, minTimeout)
	}
}
