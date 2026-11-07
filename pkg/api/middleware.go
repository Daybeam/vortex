package api

import (
	"net/http"
	"time"

	"github.com/daybeam/vortex/core/obs"
)

// statusResponseWriter wraps http.ResponseWriter to capture the status code
// and response body size for HTTP request logging.
type statusResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing.
// Without this passthrough the wrapper hides http.Flusher from SSE
// handlers (e.g. /api/chat/stream), whose `w.(http.Flusher)` assertion
// then fails and the stream dies with "streaming unsupported".
func (w *statusResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// HTTPLoggingMiddleware wraps an http.Handler with request logging.
// Each request is logged as an EventHTTPRequest event to the Logger,
// capturing method, path, status, duration, and response size.
// This makes HTTP calls visible in the SSE event stream and .jsonl log files.
func HTTPLoggingMiddleware(logger *obs.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip SSE endpoints — they are long-lived streams, not discrete requests.
			if r.URL.Path == "/api/events" || r.URL.Path == "/api/chat/stream" {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			sw := &statusResponseWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			duration := time.Since(start)

			if logger != nil {
				logger.Log(obs.EventHTTPRequest, "", "", map[string]any{
					"method":      r.Method,
					"path":        r.URL.Path,
					"status":      sw.status,
					"duration_ms": duration.Milliseconds(),
					"remote_addr": r.RemoteAddr,
					"bytes_out":   sw.bytes,
				})
			}
		})
	}
}
