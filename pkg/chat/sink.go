package chat

import (
	"context"
	"os"
)

// CapSet declares what a Sink can do. Used by the event loop to decide
// degradation rules (e.g. TG can't stream speech, so it gets a voice
// file instead of incremental audio).
//
// See docs/plan/CHAT_MODALITY_AND_ACTOR_DESIGN.md §2.2 (Patch B).
type CapSet struct {
	TextDelta     bool // can stream incremental text
	SpeechStream  bool // can play audio as it's generated (TG=false)
	Action        bool // can execute device actions
	Interrupt     bool // can be preempted by higher-priority output
	MaxMessageLen int  // TG=4096, web=0 (unlimited)
}

// Sink renders ChatEvents to a specific output channel. Each channel
// (Telegram, web SSE, future audio/desktop) implements one Sink.
//
// The event loop (forwardEvents / StreamHandler) manages the cursor,
// terminal detection, and heartbeat — Sink only handles per-event
// rendering and terminal cleanup.
//
// Render is called for each event in seq order. Flush is called once
// when the loop detects a terminal event, before exiting. For
// stateless sinks (web SSE), Flush is a no-op.
type Sink interface {
	Name() string
	Caps() CapSet
	Render(ctx context.Context, ev ChatEvent) error
	Flush() error
}

// ChatSinkEnv is the feature flag for the Sink abstraction.
// VORTEX_CHAT_SINK=legacy (default): original code path, no Sink.
// VORTEX_CHAT_SINK=sink: new code path using Sink abstraction.
const ChatSinkEnv = "VORTEX_CHAT_SINK"

// SinkEnabled reports whether the Sink-based code path is active.
func SinkEnabled() bool {
	switch os.Getenv(ChatSinkEnv) {
	case "sink":
		return true
	default:
		return false
	}
}
