package main

import (
	"net/http"
	"time"
)

// newHardenedHTTPServer creates an *http.Server with explicit timeouts to
// prevent Slowloris DoS and connection exhaustion (audit C1).
// ReadHeaderTimeout (audit O-9) is a tighter, dedicated header-read deadline:
// ReadTimeout covers the whole request read, but the header phase is the
// cheapest for an attacker to hold open, and WriteTimeout is disabled here so
// a stalled client would otherwise hold its goroutine until IdleTimeout.
// WriteTimeout stays 0 because SSE connections need unlimited write time.
func newHardenedHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // disabled — SSE connections need unlimited write time
		IdleTimeout:       120 * time.Second,
	}
}
