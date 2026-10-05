package core

import (
	"context"
	"strings"
	"time"
)

// ChatMemoryManager transforms raw chat history into a bounded-context
// history before it's sent to the provider. Implementations may apply
// sliding windows, rolling summaries, fact extraction, etc.
//
// This interface is deliberately minimal to allow third-party
// implementations (e.g., mem0, Letta, Zep) to adapt via a thin wrapper.
// The default implementation is RollingWindowMemory, which reuses the
// existing context_manager primitives (applySemanticPruning, applySessionDedup,
// applyRTK) for rule-based compression — no LLM calls required.
type ChatMemoryManager interface {
	// Process takes the full session history and returns a bounded
	// history suitable for the provider call. The returned slice
	// may be shorter than the input (windowed/summarized) but must
	// preserve conversation coherence.
	Process(ctx context.Context, sessionID string, history []ChatMessage) []ChatMessage
}

// RollingWindowMemory is the default ChatMemoryManager.
// It keeps the most recent WindowSize messages as raw text and compresses
// older messages into a single summary using existing context_manager
// primitives. When WindowSize <= 0, defaults to 10.
//
// Reuses: applySemanticPruning (head+tail turn retention),
// applySessionDedup (long-line dedup), applyRTK (timestamp/noise strip).
// All are pure functions in core/context_manager.go — no LLM calls.
type RollingWindowMemory struct {
	WindowSize int
}

func NewRollingWindowMemory(windowSize int) *RollingWindowMemory {
	if windowSize <= 0 {
		windowSize = 10
	}
	return &RollingWindowMemory{WindowSize: windowSize}
}

func (m *RollingWindowMemory) Process(_ context.Context, _ string, history []ChatMessage) []ChatMessage {
	if len(history) <= m.WindowSize {
		return history
	}

	recent := history[len(history)-m.WindowSize:]
	older := history[:len(history)-m.WindowSize]

	summary := m.summarizeOlder(older)

	result := make([]ChatMessage, 0, len(recent)+1)
	result = append(result, ChatMessage{
		Role:      "system",
		Content:   summary,
		CreatedAt: time.Now(),
	})
	result = append(result, recent...)
	return result
}

func (m *RollingWindowMemory) summarizeOlder(older []ChatMessage) string {
	var b strings.Builder
	b.WriteString("## Conversation Summary (older turns compressed)\n\n")
	for _, msg := range older {
		switch msg.Role {
		case "user":
			b.WriteString("User: " + msg.Content + "\n")
		case "assistant":
			b.WriteString("Assistant: " + msg.Content + "\n")
		default:
			b.WriteString(msg.Content + "\n")
		}
	}

	text := b.String()
	text = applyRTK(text)
	text = applySessionDedup(text)
	text = applySemanticPruning(text)

	return text
}
