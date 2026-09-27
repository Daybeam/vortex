package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// bodyTracker tracks whether each HTTP response body's Close() was called.
// Used to verify audit M14/M15/M16 fixes.
type bodyTracker struct {
	mu       sync.Mutex
	unclosed int
	total    int
}

type trackingTransport struct {
	rt  http.RoundTripper
	trk *bodyTracker
}

func (t *trackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.rt.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	t.trk.mu.Lock()
	t.trk.total++
	t.trk.unclosed++
	t.trk.mu.Unlock()
	resp.Body = &trackingBodyCloser{
		ReadCloser: resp.Body,
		trk:        t.trk,
	}
	return resp, nil
}

type trackingBodyCloser struct {
	io.ReadCloser
	trk *bodyTracker
}

func (b *trackingBodyCloser) Close() error {
	b.trk.mu.Lock()
	b.trk.unclosed--
	b.trk.mu.Unlock()
	return b.ReadCloser.Close()
}

func newTrackingClient() (*http.Client, *bodyTracker) {
	trk := &bodyTracker{}
	transport := &trackingTransport{rt: http.DefaultTransport, trk: trk}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}, trk
}

func (t *bodyTracker) assertAllClosed(tb testing.TB) {
	tb.Helper()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.unclosed != 0 {
		tb.Errorf("body leak: %d response bodies were not closed (totalBodies=%d)", t.unclosed, t.total)
	}
}

// TestAuditM15_ClaimTask_BodyClosedOnNonOK verifies that ClaimTask closes
// the HTTP response body even when the server returns a non-200 status.
// Regression test for audit M15.
func TestAuditM15_ClaimTask_BodyClosedOnNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict) // 409
	}))
	defer srv.Close()

	client, trk := newTrackingClient()
	hub := &RemoteTaskHub{
		BaseURL:    srv.URL,
		HTTPClient: client,
		notifyChan: make(chan struct{}),
		stopChan:   make(chan struct{}),
	}

	ok := hub.ClaimTask("step_1")
	if ok {
		t.Fatal("expected ClaimTask to return false on 409")
	}
	trk.assertAllClosed(t)
}

// TestAuditM15_ClaimTask_BodyClosedOnOK verifies the success path.
func TestAuditM15_ClaimTask_BodyClosedOnOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		graph := &schemas.TaskGraph{TaskID: "t1"}
		step := &schemas.Step{ID: "step_1", RoleID: "worker", Task: "do work"}
		json.NewEncoder(w).Encode(map[string]any{"graph": graph, "step": step})
	}))
	defer srv.Close()

	client, trk := newTrackingClient()
	hub := &RemoteTaskHub{
		BaseURL:    srv.URL,
		HTTPClient: client,
		notifyChan: make(chan struct{}),
		stopChan:   make(chan struct{}),
	}

	ok := hub.ClaimTask("step_1")
	if !ok {
		t.Fatal("expected ClaimTask to return true on 200")
	}
	trk.assertAllClosed(t)
}

// TestAuditM16_SensingGlobal_BodyClosedOnNonOK verifies that SensingGlobal
// closes the HTTP response body even when the server returns a non-200 status.
// Regression test for audit M16.
func TestAuditM16_SensingGlobal_BodyClosedOnNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // 503
	}))
	defer srv.Close()

	client, trk := newTrackingClient()
	hub := &RemoteTaskHub{
		BaseURL:    srv.URL,
		HTTPClient: client,
		notifyChan: make(chan struct{}),
		stopChan:   make(chan struct{}),
	}
	sf := &RemoteSignalField{Hub: hub}

	ids := sf.SensingGlobal(5, 0)
	if ids != nil {
		t.Fatalf("expected nil on 503, got %v", ids)
	}
	trk.assertAllClosed(t)
}

// TestAuditM16_SensingGlobal_BodyClosedOnOK verifies the success path.
func TestAuditM16_SensingGlobal_BodyClosedOnOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]string{"task_a", "task_b"})
	}))
	defer srv.Close()

	client, trk := newTrackingClient()
	hub := &RemoteTaskHub{
		BaseURL:    srv.URL,
		HTTPClient: client,
		notifyChan: make(chan struct{}),
		stopChan:   make(chan struct{}),
	}
	sf := &RemoteSignalField{Hub: hub}

	ids := sf.SensingGlobal(5, 0)
	if len(ids) != 2 {
		t.Fatalf("expected 2 task IDs, got %d", len(ids))
	}
	trk.assertAllClosed(t)
}

// TestAuditM14_ExecuteTask_YieldBodyClosed verifies that ExecuteTask closes
// the yield HTTP response body. Regression test for audit M14.
// Uses a minimal Spawner setup; Spawn may fail but the yield call still executes.
func TestAuditM14_ExecuteTask_YieldBodyClosed(t *testing.T) {
	yieldCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/swarm/claim":
			graph := &schemas.TaskGraph{TaskID: "t1"}
			step := &schemas.Step{ID: "step_1", RoleID: "worker", Task: "do work"}
			json.NewEncoder(w).Encode(map[string]any{"graph": graph, "step": step})
		case "/api/swarm/yield":
			yieldCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, trk := newTrackingClient()
	hub := &RemoteTaskHub{
		BaseURL:    srv.URL,
		HTTPClient: client,
		notifyChan: make(chan struct{}),
		stopChan:   make(chan struct{}),
	}

	// Claim to populate currentStep/currentGraph
	if !hub.ClaimTask("step_1") {
		t.Fatal("ClaimTask failed")
	}

	// ExecuteTask calls Spawn then yields. Spawn needs a Spawner with at
	// least a logger and registry to not panic. Spawn will likely return an
	// error (empty registry), but ExecuteTask still proceeds to yield.
	tmpDir := t.TempDir()
	logger, err := NewLogger(tmpDir, &config.SystemSettings{})
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	hub.Spawner = &Spawner{
		registry: &config.Registry{
			Providers: make(map[string]*config.ProviderConfig),
			Roles:     make(map[string]*config.Role),
		},
		logger: logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// fixes audit T-C09: the original test had recover() that swallowed ALL
	// panics and t.Skip("verified by code inspection") if yield was never
	// reached — making the test pass vacuously. Now we fail if yield is not
	// reached, and only recover to add context to the failure message.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Spawn panicked before yield (M14 body-close not exercised): %v", r)
		}
		if !yieldCalled {
			t.Fatal("yield was never reached — M14 body-close guarantee not exercised")
		}
		trk.assertAllClosed(t)
	}()

	_, _ = hub.ExecuteTask(ctx, "step_1")
}
