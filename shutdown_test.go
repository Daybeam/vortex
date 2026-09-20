//go:build !web && !dev && !desktop

package main

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

// mockCloser tracks whether Close() was called.
type mockCloser struct {
	closed bool
}

func (m *mockCloser) Close() error {
	m.closed = true
	return nil
}

// TestWaitForServerOrSignal_SignalTriggersCleanup verifies that when a signal
// is received, rootCancel is called and stdin is closed to unblock the server.
// This is the core of the H7 fix: without signal handling, SIGINT kills the
// process immediately and all deferred cleanup is skipped.
func TestWaitForServerOrSignal_SignalTriggersCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stdin := &mockCloser{}

	serverDone := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)

	// Simulate a server that blocks until stdin is closed (like ServeStdio).
	go func() {
		// In real code, ServeStdio blocks on reading stdin.
		// When stdin is closed, it returns. Simulate this with a short sleep.
		time.Sleep(50 * time.Millisecond)
		serverDone <- nil
	}()

	// Send a signal after 10ms.
	go func() {
		time.Sleep(10 * time.Millisecond)
		sigCh <- os.Interrupt
	}()

	err := waitForServerOrSignal(serverDone, sigCh, cancel, stdin)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	// Verify rootCancel was called (context should be cancelled).
	if ctx.Err() == nil {
		t.Fatal("rootCancel was not called — context is not cancelled after signal")
	}

	// Verify stdin was closed.
	if !stdin.closed {
		t.Fatal("stdin was not closed — server would remain blocked on stdin read")
	}
}

// TestWaitForServerOrSignal_ServerFinishesFirst verifies that when the server
// finishes before any signal, the function returns the server's error without
// calling rootCancel or closing stdin.
func TestWaitForServerOrSignal_ServerFinishesFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &mockCloser{}

	serverDone := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)

	// Server finishes immediately with no error.
	serverDone <- nil

	err := waitForServerOrSignal(serverDone, sigCh, cancel, stdin)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	// rootCancel should NOT have been called (no signal).
	if ctx.Err() != nil {
		t.Fatal("rootCancel should not be called when server finishes first")
	}

	// stdin should NOT have been closed.
	if stdin.closed {
		t.Fatal("stdin should not be closed when server finishes first")
	}
}

// TestWaitForServerOrSignal_ServerErrorPropagates verifies that server errors
// are propagated to the caller.
func TestWaitForServerOrSignal_ServerErrorPropagates(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &mockCloser{}

	serverDone := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)

	serverDone <- io.ErrClosedPipe

	err := waitForServerOrSignal(serverDone, sigCh, cancel, stdin)
	if err == nil {
		t.Fatal("expected error to be propagated, got nil")
	}
}
