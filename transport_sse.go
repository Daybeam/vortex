//go:build !web && !dev && !desktop

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// serveSSE runs the full MCP server over SSE + Streamable HTTP as an
// alternative to stdio when stdio transport is unreliable. It is the raw
// transport only: no auth, no public/admin tiering, and the same full tool
// surface as stdio. The SaaS tiering lives in the private enterprise layer
// (transport_sse_auth.go is not published).
// rootCancel cancels the root context so background goroutines (scheduler,
// spawner, etc.) stop before the HTTP server shuts down (audit L4).
func serveSSE(mcpServer *server.MCPServer, rootCancel context.CancelFunc) {
	host := getenv("VORTEX_HOST", "0.0.0.0")
	port := getenv("VORTEX_PORT", "8000")
	addr := "0.0.0.0:" + port

	// BaseURL is resolved from VORTEX_BASE_URL (set by the caller) so
	// that tunnel / reverse-proxy URLs work, falling back to localhost.
	baseURL := os.Getenv("VORTEX_BASE_URL")
	if baseURL == "" {
		if host == "0.0.0.0" || host == "localhost" || host == "127.0.0.1" {
			baseURL = fmt.Sprintf("http://localhost:%s", port)
		} else {
			baseURL = "https://" + host
		}
	}

	fmt.Fprintf(os.Stderr, "[vortex] SSE mode on %s (BaseURL: %s)\n", addr, baseURL)

	sse := server.NewSSEServer(
		mcpServer,
		server.WithBaseURL(baseURL),
		server.WithUseFullURLForMessageEndpoint(true),
		server.WithAppendQueryToMessageEndpoint(),
	)
	streamServer := server.NewStreamableHTTPServer(mcpServer)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/sse", sse.SSEHandler())
	mux.Handle("/message", sse.MessageHandler())
	mux.Handle("/mcp", streamServer)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// Don't use log.Fatalf — os.Exit skips deferred cleanup in
			// main.go (CloseMCPConnections, JITSessions.CloseAll, etc.).
			// Log the error and trigger graceful shutdown instead.
			log.Printf("[vortex] SSE server error: %v", err)
			stop()
		}
	}()

	<-sigCtx.Done()
	log.Println("[vortex] shutdown signal received, draining connections...")
	rootCancel() // audit L4: stop background goroutines before HTTP shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[vortex] server shutdown error: %v", err)
	}
}
