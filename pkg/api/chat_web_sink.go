package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/daybeam/vortex/pkg/chat"
)

// webSink implements chat.Sink for the web SSE channel.
// Each event is written immediately as an SSE frame — no buffering,
// no terminal cleanup. Flush is a no-op.
//
// See docs/plan/CHAT_MODALITY_AND_ACTOR_DESIGN.md §2.2 (Patch B).
type webSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newWebSink(w http.ResponseWriter, flusher http.Flusher) *webSink {
	return &webSink{w: w, flusher: flusher}
}

func (s *webSink) Name() string { return "web-sse" }

func (s *webSink) Caps() chat.CapSet {
	return chat.CapSet{
		TextDelta:     true,
		SpeechStream:  true,
		Action:        false,
		Interrupt:     true,
		MaxMessageLen: 0,
	}
}

func (s *webSink) Render(_ context.Context, ev chat.ChatEvent) error {
	writeSSE(s.w, ev)
	return nil
}

func (s *webSink) Flush() error { return nil }

// streamHandlerWithSink is the Sink-based replacement for StreamHandler's
// event loop. Behavior is identical to StreamHandler; activated by
// VORTEX_CHAT_SINK=sink.
func (h *ChatHandler) streamHandlerWithSink(w http.ResponseWriter, r *http.Request, sessionID string, lastSeq int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	session := h.Store.Get(sessionID)
	if session == nil {
		return
	}

	sink := newWebSink(w, flusher)

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
			_ = sink.Render(r.Context(), ev)
			if chat.IsTerminalEvent(ev.Type, false) {
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
