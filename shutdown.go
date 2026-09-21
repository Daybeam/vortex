package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// waitForServerOrSignal waits for either the MCP stdio server to finish or a
// shutdown signal. On signal, it calls rootCancel to stop background
// goroutines and closes stdin to unblock the server's stdin reader,
// then waits for the server to return cleanly. This allows deferred
// cleanup functions (scheduler.Stop, logger.Close, JIT CloseAll, etc.)
// to run before the process exits.
//
// Returns the server's error (nil if the server returned cleanly after
// a signal-driven shutdown).
//
// audit H7/C6: without this, SIGINT/SIGTERM kills the process immediately
// and all deferred cleanup is skipped, leaking goroutines, subprocesses,
// and unflushed log buffers.
func waitForServerOrSignal(serverDone <-chan error, sigCh <-chan os.Signal, rootCancel context.CancelFunc, stdin io.Closer) error {
	select {
	case <-sigCh:
		fmt.Fprintln(os.Stderr, "[vortex] shutdown signal received, cleaning up...")
		rootCancel()
		stdin.Close() // unblock ServeStdio's stdin reader
		return <-serverDone
	case err := <-serverDone:
		return err
	}
}

// waitForHTTPSignal starts an HTTP server in a goroutine and waits for
// either the server to fail or a shutdown signal. On signal, it calls
// rootCancel and gracefully shuts down the HTTP server (draining
// in-flight requests with a 10s timeout), then returns.
//
// audit C6: without this, SIGINT/SIGTERM kills the process immediately
// and all deferred cleanup is skipped.
func waitForHTTPSignal(srv *http.Server, rootCancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-sigCh:
		fmt.Fprintln(os.Stderr, "[vortex] shutdown signal received, cleaning up...")
		rootCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx) // gracefully drain in-flight requests
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[vortex] HTTP server error: %v\n", err)
		}
		rootCancel()
	}
}
