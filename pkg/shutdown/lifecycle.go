package shutdown

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// LifecycleController abstracts how process shutdown is triggered.
//
// On Linux/desktop, shutdown is triggered by POSIX signals (SIGINT, SIGTERM).
// On Android, the OS does not deliver POSIX signals to app processes —
// the shell layer calls an HTTP endpoint (POST /api/lifecycle) which
// triggers the same graceful shutdown path.
//
// This is the ONLY platform-specific abstraction in the shutdown package
// (per ANDROID_PORT_ANALYSIS.md §1.2). All other shutdown logic is
// platform-agnostic.
type LifecycleController interface {
	// ShutdownCh returns a channel that receives a human-readable reason
	// string when a shutdown trigger occurs. The channel is buffered (size 1)
	// so the sender never blocks.
	ShutdownCh() <-chan string

	// Stop releases any resources (signal handlers, HTTP listeners).
	Stop()
}

// SignalLifecycleController implements LifecycleController using POSIX signals.
// This is the default for Linux/desktop builds.
type SignalLifecycleController struct {
	ch      chan string
	stopCh  chan struct{}
	stopped bool
	mu      sync.Mutex
}

func NewSignalLifecycleController() *SignalLifecycleController {
	s := &SignalLifecycleController{
		ch:     make(chan string, 1),
		stopCh: make(chan struct{}),
	}
	go s.watch()
	return s
}

func (s *SignalLifecycleController) watch() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		s.ch <- fmt.Sprintf("signal:%v", sig)
	case <-s.stopCh:
	}
}

func (s *SignalLifecycleController) ShutdownCh() <-chan string { return s.ch }

func (s *SignalLifecycleController) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		close(s.stopCh)
		s.stopped = true
	}
}

// HTTPLifecycleController implements LifecycleController via an HTTP endpoint.
// Used by Android builds where the shell layer (Kotlin/Compose) calls
// POST /api/lifecycle {event: "stop"} to trigger graceful shutdown.
//
// The HTTP server is embedded — no external listener is started. The
// controller exposes a HandlerFunc that the main route mux mounts at
// /api/lifecycle. When the handler receives a "stop" event, it signals
// the ShutdownCh.
type HTTPLifecycleController struct {
	ch      chan string
	stopCh  chan struct{}
	stopped bool
	mu      sync.Mutex
}

func NewHTTPLifecycleController() *HTTPLifecycleController {
	return &HTTPLifecycleController{
		ch:     make(chan string, 1),
		stopCh: make(chan struct{}),
	}
}

func (h *HTTPLifecycleController) ShutdownCh() <-chan string { return h.ch }

func (h *HTTPLifecycleController) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopped {
		close(h.stopCh)
		h.stopped = true
	}
}

// HandlerFunc returns an http.HandlerFunc that the route mux mounts at
// /api/lifecycle. A POST with body {"event":"stop"} triggers shutdown.
// Other events (trim_memory, low_storage) are accepted but only "stop"
// initiates graceful shutdown — the others are informational and logged.
func (h *HTTPLifecycleController) HandlerFunc() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var body struct {
			Event string `json:"event"`
		}
		if err := jsonDecode(r, &body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		switch body.Event {
		case "stop":
			select {
			case h.ch <- "http:stop":
			default:
				// already signaled — no-op
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintln(w, `{"status":"accepted"}`)
		case "trim_memory", "low_storage":
			// Informational events — logged but do not trigger shutdown.
			fmt.Fprintf(os.Stderr, "[lifecycle] received event: %s\n", body.Event)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintln(w, `{"status":"acknowledged"}`)
		default:
			http.Error(w, "unknown event: "+body.Event, http.StatusBadRequest)
		}
	}
}

// jsonDecode decodes the request body into v using encoding/json.
func jsonDecode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}
