package main

import (
	"net/http"
	"testing"
	"time"
)

// TestNewHardenedHTTPServer_HasTimeouts is the regression test for audit C1:
// HTTP servers in consumer/desktop editions must have non-zero ReadTimeout
// and IdleTimeout to prevent Slowloris DoS and connection exhaustion.
// WriteTimeout is intentionally 0 to allow SSE/streaming connections
// (see http_server_config.go comment). This test fails if anyone reverts
// to http.ListenAndServe (which uses zero-value timeouts) or removes the
// timeout configuration.
func TestNewHardenedHTTPServer_HasTimeouts(t *testing.T) {
	srv := newHardenedHTTPServer("127.0.0.1:0", http.NewServeMux())

	if srv.ReadTimeout == 0 {
		t.Error("ReadTimeout is zero — vulnerable to Slowloris DoS (audit C1)")
	}
	if srv.IdleTimeout == 0 {
		t.Error("IdleTimeout is zero — keep-alive connections never close (audit C1)")
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

// TestDesktopCORSOrigin_AllowsWailsWebview is the regression test for the
// desktop CORS outage. The Wails webview origin (http://wails.localhost on
// Windows, wails://wails on macOS/Linux) is cross-origin to the loopback API
// and WebView2 enforces CORS, so desktop responses must always carry an
// Access-Control-Allow-Origin header. Bug: audit S-M4 made applyCORS
// env-gated, emitting no header when VORTEX_CORS_ORIGIN was unset, which
// silently blocked every desktop /api call (surfaced to the user as
// "ROI metrics unavailable" / "Infrastructure alerts unavailable" toasts).
func TestDesktopCORSOrigin_AllowsWailsWebview(t *testing.T) {
	// No env override → must default to "*" because the Wails webview origin
	// differs per platform; a fixed value would break macOS/Linux.
	t.Setenv("VORTEX_CORS_ORIGIN", "")
	resetCORSOriginCache()
	if got := desktopCORSOrigin(); got != "*" {
		t.Fatalf("desktop CORS must default to \"*\" so the Wails webview can reach the API; got %q", got)
	}

	// An explicit override is still honoured.
	t.Setenv("VORTEX_CORS_ORIGIN", "http://example.com")
	resetCORSOriginCache()
	if got := desktopCORSOrigin(); got != "http://example.com" {
		t.Fatalf("desktop CORS must honour VORTEX_CORS_ORIGIN; got %q", got)
	}
}
