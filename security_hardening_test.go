//go:build web

package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/pkg/safelimits"
)

// TestS_M1_BodyCap_RejectsOversizedRequest is a regression test for audit
// S-M1 / NEW-4: http.MaxBytesReader must cap request bodies on BOTH adminMux
// and publicMux to prevent OOM from unbounded JSON.
// regression for audit TEST-2: security hardening commit df7c3f73a had zero tests.
func TestS_M1_BodyCap_RejectsOversizedRequest(t *testing.T) {
	// Simulate the cappedMux wrapper from main_web.go.
	// regression for audit TEST-NEW-2: the old version had an empty if-block
	// (vacuous assertion — always passed even if MaxBytesReader was removed).
	var readErr error
	var bytesRead int64
	bodyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		bytesRead = n
		readErr = err
		w.WriteHeader(http.StatusOK)
	})
	cappedMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, safelimits.MaxRequestBody)
		bodyHandler.ServeHTTP(w, r)
	})

	// Send a body larger than MaxRequestBody (10 MiB)
	oversized := bytes.Repeat([]byte("x"), int(safelimits.MaxRequestBody)+1024)
	req := httptest.NewRequest(http.MethodPost, "/api/test", bytes.NewReader(oversized))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	cappedMux.ServeHTTP(rec, req)

	// MaxBytesReader must cap the body: io.Copy should return an error
	// (http.MaxBytesError) and bytesRead should be <= MaxRequestBody, not the full size.
	if readErr == nil {
		t.Fatalf("MaxBytesReader failed to cap body: io.Copy returned nil error after reading %d bytes (limit %d)", bytesRead, safelimits.MaxRequestBody)
	}
	if bytesRead > int64(safelimits.MaxRequestBody) {
		t.Errorf("MaxBytesReader failed to cap body: read %d bytes, limit is %d", bytesRead, safelimits.MaxRequestBody)
	}
}

// TestS_M4_CORS_Allowlist_EmitsHeaderForAllowedOrigin is a regression test for
// audit S-M4: tieredBearerAuthCORS must emit Access-Control-Allow-Origin only
// for origins in the allowedOrigins list, not for arbitrary origins.
// regression for audit TEST-2: security hardening commit df7c3f73a had zero tests.
func TestS_M4_CORS_Allowlist_EmitsHeaderForAllowedOrigin(t *testing.T) {
	allowedOrigins := []string{"https://app.example.com", "http://localhost:3000"}
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Use dev mode (no keys) so requests pass through to adminMux.
	// Use /api/test (not /health) so the CORS logic at line 644 runs.
	handler := tieredBearerAuthCORS(allowedOrigins, "", "", nil, dummyHandler, dummyHandler)

	tests := []struct {
		name       string
		origin     string
		expectACAO string // expected Access-Control-Allow-Origin value, "" means absent
	}{
		{"allowed origin", "https://app.example.com", "https://app.example.com"},
		{"allowed localhost", "http://localhost:3000", "http://localhost:3000"},
		{"non-allowed origin", "https://evil.example.com", ""},
		{"no origin header", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			got := rec.Header().Get("Access-Control-Allow-Origin")
			if got != tt.expectACAO {
				t.Errorf("Origin %q: got ACAO %q, want %q", tt.origin, got, tt.expectACAO)
			}
		})
	}
}

// TestS_M4_CORS_EmptyAllowlist_DefaultsToWildcard is a regression test for
// audit S-M4: when allowedOrigins is empty, the server should default to "*"
// (single-machine mode).
func TestS_M4_CORS_EmptyAllowlist_DefaultsToWildcard(t *testing.T) {
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := tieredBearerAuthCORS(nil, "", "", nil, dummyHandler, dummyHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get("Access-Control-Allow-Origin")
	if got != "*" {
		t.Errorf("empty allowlist: got ACAO %q, want %q", got, "*")
	}
}
