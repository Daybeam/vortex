package extreme_tests

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestExtreme_SSE_MidStreamDisconnect verifies that an SSE stream
// can handle a client disconnecting mid-stream and reconnecting
// with Last-Event-ID to resume from where it left off.
//
// Extreme scenario: Client network drops during event stream,
// then reconnects and expects to see subsequent events.
func TestExtreme_SSE_MidStreamDisconnect(t *testing.T) {
	// Track which events each "client" received
	var mu sync.Mutex
	receivedEvents := []int{}

	// SSE server that sends events 1-100 with 10ms delay
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not support flushing")
			return
		}

		// Parse Last-Event-ID to resume from
		lastEventID := 0
		if leid := r.Header.Get("Last-Event-ID"); leid != "" {
			fmt.Sscanf(leid, "evt-%d", &lastEventID)
		}

		for i := lastEventID + 1; i <= 100; i++ {
			select {
			case <-r.Context().Done():
				return // client disconnected
			default:
			}
			fmt.Fprintf(w, "id: evt-%d\nevent: update\ndata: {\"id\":%d}\n\n", i, i)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer server.Close()

	// Client 1: connect and read first 5 events, then disconnect
	ctx1, cancel1 := context.WithCancel(context.Background())
	req1, _ := http.NewRequestWithContext(ctx1, "GET", server.URL, nil)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("client 1 connect: %v", err)
	}

	scanner1 := bufio.NewScanner(resp1.Body)
	lastReceived := 0
	for scanner1.Scan() {
		line := scanner1.Text()
		if strings.HasPrefix(line, "id: evt-") {
			fmt.Sscanf(line, "id: evt-%d", &lastReceived)
			mu.Lock()
			receivedEvents = append(receivedEvents, lastReceived)
			mu.Unlock()
			if lastReceived >= 5 {
				break
			}
		}
	}
	cancel1()
	resp1.Body.Close()

	if lastReceived < 5 {
		t.Fatalf("client 1 only received %d events before disconnect, expected ≥5", lastReceived)
	}

	// Client 2: reconnect with Last-Event-ID and verify we get subsequent events
	req2, _ := http.NewRequest("GET", server.URL, nil)
	req2.Header.Set("Last-Event-ID", fmt.Sprintf("evt-%d", lastReceived))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("client 2 reconnect: %v", err)
	}
	defer resp2.Body.Close()

	scanner2 := bufio.NewScanner(resp2.Body)
	resumed := 0
	for scanner2.Scan() {
		line := scanner2.Text()
		if strings.HasPrefix(line, "id: evt-") {
			var id int
			fmt.Sscanf(line, "id: evt-%d", &id)
			mu.Lock()
			receivedEvents = append(receivedEvents, id)
			mu.Unlock()
			resumed++
			if id <= lastReceived {
				t.Errorf("client 2 received old event %d (lastReceived=%d)", id, lastReceived)
			}
			if resumed >= 5 {
				break
			}
		}
	}

	if resumed == 0 {
		t.Error("client 2 received 0 events after reconnect with Last-Event-ID")
	}
}

// TestExtreme_HTTPServer_GracefulShutdown verifies that an HTTP server
// drains in-flight requests on shutdown rather than killing them abruptly.
//
// Extreme scenario: SIGTERM arrives while a long-running request is in flight.
func TestExtreme_HTTPServer_GracefulShutdown(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCompleted := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		time.Sleep(200 * time.Millisecond) // simulate work
		w.Write([]byte("done"))
		close(requestCompleted)
	}))

	// Start a request in a goroutine
	go func() {
		resp, _ := http.Get(server.URL)
		if resp != nil {
			resp.Body.Close()
		}
	}()

	// Wait for request to start
	<-requestStarted

	// Initiate graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	server.Config.Shutdown(shutdownCtx)

	// Verify the in-flight request completed
	select {
	case <-requestCompleted:
		// Success — request drained
	case <-time.After(3 * time.Second):
		t.Error("in-flight request was killed instead of drained on shutdown")
	}
}
