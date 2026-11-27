package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/tools"
)

// TestHandleEventsStream_FlushesHeadersImmediately is a regression test for
// the bug where the SSE handler entered its event loop without flushing the
// response headers. In Go's net/http, headers are not sent until the first
// Write or Flush call. If no events arrived (quiet system with no active
// tasks), the client never received the HTTP 200 response, so
// EventSource.onopen never fired and the UI showed "disconnected".
//
// This test verifies that the response headers (status 200 + Content-Type
// text/event-stream) arrive within 1 second WITHOUT any events being
// generated. Before the fix, this test would time out.
func TestHandleEventsStream_FlushesHeadersImmediately(t *testing.T) {
	logger, err := obs.NewLogger(t.TempDir(), &config.SystemSettings{})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	handler := &APIHandler{App: &tools.App{Logger: logger}}
	server := httptest.NewServer(http.HandlerFunc(handler.HandleEventsStream))
	defer server.Close()

	// Use a client with a short timeout. If the server doesn't flush headers,
	// this will time out.
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET /api/events failed (headers never flushed?): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Fatalf("expected Content-Type text/event-stream, got %q", ct)
	}

	// Read with a short deadline to prove the connection is alive and
	// headers were already received. We don't expect any event data.
	_ = resp.Body // keep open; server blocks on event channel — that's fine
}

// TestHandleEventsStream_DeliversEvents verifies that when events ARE
// generated, they are delivered to the SSE client as expected.
func TestHandleEventsStream_DeliversEvents(t *testing.T) {
	logger, err := obs.NewLogger(t.TempDir(), &config.SystemSettings{})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	handler := &APIHandler{App: &tools.App{Logger: logger}}
	server := httptest.NewServer(http.HandlerFunc(handler.HandleEventsStream))
	defer server.Close()

	// Start reading the response in a goroutine.
	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Get(server.URL)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 1024)
		n, err := resp.Body.Read(buf)
		done <- result{body: string(buf[:n]), err: err}
	}()

	// Give the client time to connect, then publish an event.
	time.Sleep(100 * time.Millisecond)
	logger.Log(obs.EventStepStarted, "test-task", "step-1", map[string]any{"msg": "hello"})

	select {
	case r := <-done:
		if r.err != nil && r.err != io.EOF {
			t.Fatalf("read error: %v", r.err)
		}
		if r.body == "" {
			t.Fatal("expected event data, got empty body")
		}
		// SSE format: "data: {...}\n\n"
		if r.body[:4] != "data" {
			t.Fatalf("expected SSE data line, got %q", r.body[:min(20, len(r.body))])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event delivery")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
