package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/tools"
)

func (h *ChatHandler) SendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		SessionID      string           `json:"session_id"`
		MessageID      string           `json:"message_id"`
		Content        string           `json:"content"`
		ParentID       string           `json:"parent_id"`
		InputDir       string           `json:"input_dir"`
		ModelOverride  string           `json:"model_override"`
		RoleID         string           `json:"role_id"`
		DirectTask     bool             `json:"direct_task"`
		ReplyTo        string           `json:"reply_to"`
		ReplyToContent string           `json:"reply_to_content"`
		Attachments    []AttachmentSpec `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	resp := h.ProcessMessage(r.Context(), ChatRequest{
		SessionID:      body.SessionID,
		MessageID:      body.MessageID,
		Content:        body.Content,
		ParentID:       body.ParentID,
		InputDir:       body.InputDir,
		ModelOverride:  body.ModelOverride,
		RoleID:         body.RoleID,
		DirectTask:     body.DirectTask,
		CallerID:       tools.CallerIdentity(r.Context()),
		ReplyTo:        body.ReplyTo,
		ReplyToContent: body.ReplyToContent,
		Attachments:    body.Attachments,
	})

	if resp.StatusCode != 0 {
		http.Error(w, resp.Error, resp.StatusCode)
		return
	}
	if resp.Error != "" {
		writeJSON(w, map[string]any{"session_id": resp.SessionID, "error": resp.Error})
		return
	}
	writeJSON(w, resp)
}

func (h *ChatHandler) StreamHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	session := h.Store.Get(sessionID)
	if session == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	lastSeq := parseSeq(r.Header.Get("Last-Event-ID"))
	// The Last-Event-ID header is authoritative on browser auto-reconnects
	// (it always carries the latest seen seq). The query param is only a
	// fallback for the initial connect, where no header exists yet — letting
	// it override would replay the whole history on every reconnect and
	// duplicate tool-call entries in the UI.
	if lastSeq == 0 {
		if q := r.URL.Query().Get("last_event_id"); q != "" {
			lastSeq = parseSeq(q)
		}
	}

	if chat.SinkEnabled() {
		h.streamHandlerWithSink(w, r, sessionID, lastSeq)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	lastPing := time.Now()
	for {
		events := session.EventsAfter(lastSeq)
		terminal := false
		for _, ev := range events {
			lastSeq = ev.Seq
			writeSSE(w, ev)
			if ev.Type == "done" || ev.Type == "error" {
				terminal = true
			}
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		if terminal {
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if time.Since(lastPing) > 15*time.Second {
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
				lastPing = time.Now()
			}
		}
	}
}
