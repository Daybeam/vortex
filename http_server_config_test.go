package main

import (
	"net/http"
	"testing"
	"time"
)

// TestNewHardenedHTTPServer_HasTimeouts is the regression test for audit C1:
// the HTTP server must have non-zero ReadHeaderTimeout, ReadTimeout and
// IdleTimeout to prevent Slowloris DoS and connection exhaustion. WriteTimeout
// is intentionally 0 to allow SSE/streaming connections (see
// http_server_config.go comment). This test fails if anyone reverts to
// http.ListenAndServe (which uses zero-value timeouts) or drops the timeouts.
func TestNewHardenedHTTPServer_HasTimeouts(t *testing.T) {
	srv := newHardenedHTTPServer("127.0.0.1:0", http.NewServeMux())

	if srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is zero — the Slowloris header phase is unbounded (audit O-9)")
	}
	if srv.ReadTimeout == 0 {
		t.Error("ReadTimeout is zero — vulnerable to Slowloris DoS (audit C1)")
	}
	if srv.IdleTimeout == 0 {
		t.Error("IdleTimeout is zero — keep-alive connections never close (audit C1)")
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (SSE streams need unlimited write time)", srv.WriteTimeout)
	}

	// Sanity: values should be reasonable (at least 10s each)
	minTimeout := 10 * time.Second
	if srv.ReadTimeout < minTimeout {
		t.Errorf("ReadTimeout %v is too short (minimum %v)", srv.ReadTimeout, minTimeout)
	}
	if srv.IdleTimeout < minTimeout {
		t.Errorf("IdleTimeout %v is too short (minimum %v)", srv.IdleTimeout, minTimeout)
	}
}
