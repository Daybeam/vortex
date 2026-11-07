package api

import (
	"context"
	"net/http"
)

// HTTPChannel wraps the existing HTTP chat routes. It is always enabled
// and registers REST endpoints on Start. Stop is a no-op — routes are
// cleaned up when the server shuts down.
type HTTPChannel struct {
	handler *ChatHandler
	mux     *http.ServeMux
}

func NewHTTPChannel(h *ChatHandler, mux *http.ServeMux) *HTTPChannel {
	return &HTTPChannel{handler: h, mux: mux}
}

func (c *HTTPChannel) Name() string { return "http" }

func (c *HTTPChannel) Start(_ context.Context) error {
	c.mux.HandleFunc("/api/chat/send", c.handler.SendHandler)
	c.mux.HandleFunc("/api/chat/upload", c.handler.UploadHandler)
	c.mux.HandleFunc("/api/chat/stream", c.handler.StreamHandler)
	c.mux.HandleFunc("/api/chat/artifacts", c.handler.ArtifactsHandler)
	c.mux.HandleFunc("/api/chat/artifact", c.handler.ArtifactHandler)
	c.mux.HandleFunc("/api/chat/history", c.handler.HistoryHandler)
	c.mux.HandleFunc("/api/chat/sessions", c.handler.SessionsHandler)
	return nil
}

func (c *HTTPChannel) Stop() {}
