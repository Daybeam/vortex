package main

import (
	"net/http"
	"time"
)

// newHardenedHTTPServer creates an *http.Server with explicit timeouts to
// prevent Slowloris DoS and connection exhaustion (audit C1).
// Values match transport_sse.go for consistency across all editions.
func newHardenedHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
}
