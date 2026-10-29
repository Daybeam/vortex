package main

import (
	"net/http"
	"os"
	"sync"
	"time"
)

// maxRequestBodyBytes limits JSON POST bodies to 10MB to prevent memory
// exhaustion DoS (audit S-M1). Legitimate orchestrator requests are well
// under this limit; larger transfers use chunked deploy endpoints.
const maxRequestBodyBytes = 10 << 20 // 10MB

// corsOriginOnce/corsOriginValue cache VORTEX_CORS_ORIGIN for the process
// lifetime (audit P-28-1). The env var is process-scoped and static; reading
// it via os.Getenv on every HTTP request is a per-request syscall. Mirrors the
// P-24-1 fix (pkg/oauth/middleware.go captures at construction) and the
// config/asset_gate.go sync.Once + reset pattern for test isolation.
var (
	corsOriginOnce  sync.Once
	corsOriginValue string
)

// corsAllowedOrigin is read from VORTEX_CORS_ORIGIN env var.
// If empty, CORS headers are not set (same-origin only — safest default).
// If set to a specific origin (e.g. "https://app.example.com"), only that
// origin is allowed. The wildcard "*" is supported but discouraged in prod.
func corsAllowedOrigin() string {
	corsOriginOnce.Do(func() {
		corsOriginValue = os.Getenv("VORTEX_CORS_ORIGIN")
	})
	return corsOriginValue
}

// resetCORSOriginCache resets the cached CORS origin. Test-only — production
// never needs to reset because the env var is process-scoped. Mirrors
// config/asset_gate.go's resetAssetGate (audit T-27-1 lesson: tests that
// change the env var must reset the cache to avoid sync.Once poisoning).
func resetCORSOriginCache() {
	corsOriginOnce = sync.Once{}
	corsOriginValue = ""
}

// applyCORS sets the Access-Control-Allow-Origin header from config (audit S-M4).
// Replaces the previous wildcard "*" which allowed any website to make
// cross-origin requests. If VORTEX_CORS_ORIGIN is unset, no header
// is emitted (same-origin only — safest default).
func applyCORS(w http.ResponseWriter) {
	if origin := corsAllowedOrigin(); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
}

// desktopCORSOrigin returns the Access-Control-Allow-Origin value for the
// desktop edition. The Wails webview runs at http://wails.localhost (Windows)
// or wails://wails (macOS/Linux), which is cross-origin to the loopback API on
// http://localhost:<port>, and WebView2 enforces CORS. Unlike applyCORS (audit
// S-M4), which emits no header unless VORTEX_CORS_ORIGIN is set, this
// loopback-only, auth-free server defaults to "*" so the UI can reach the API.
func desktopCORSOrigin() string {
	if origin := corsAllowedOrigin(); origin != "" {
		return origin
	}
	return "*"
}

// maxBytesMiddleware wraps an http.Handler with http.MaxBytesReader to
// reject request bodies exceeding maxRequestBodyBytes.
func maxBytesMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// newHardenedHTTPServer creates an *http.Server with explicit timeouts to
// prevent Slowloris DoS and connection exhaustion (audit C1), plus a
// body-size limit to prevent memory exhaustion DoS (audit S-M1).
// Values match transport_sse.go for consistency across all editions.
//
// FIX (2026-10-09): WriteTimeout set to 0 (disabled). A 60s WriteTimeout
// kills SSE/streaming connections — Go's WriteTimeout is a deadline for the
// ENTIRE response, not per-write. SSE connections are long-lived (minutes/
// hours) and get forcefully terminated after 60s, causing client disconnects
// and potential panics in SSE handlers. ReadTimeout (30s) remains the
// primary Slowloris defense; IdleTimeout (120s) handles idle connections.
//
// FIX (audit O-9): ReadHeaderTimeout (10s) is set explicitly. ReadTimeout
// covers the whole request read, but the header-read phase is the cheapest for
// an attacker to hold open; a dedicated, tighter header deadline is the
// standard Slowloris defense. This matters most here because WriteTimeout is
// intentionally disabled, so a stalled client otherwise holds its goroutine
// until IdleTimeout.
func newHardenedHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           maxBytesMiddleware(handler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // disabled — SSE connections need unlimited write time
		IdleTimeout:       120 * time.Second,
	}
}
