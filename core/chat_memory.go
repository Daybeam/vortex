package core

import (
	"context"
	"fmt"
	"strings"
)

// ChatMemoryManager transforms raw chat history into a bounded-context history
// before it is sent to the provider. Implementations may apply sliding
// windows, rolling summaries, etc.
type ChatMemoryManager interface {
	Process(ctx context.Context, sessionID string, history []ChatMessage) []ChatMessage
}

// RollingWindowMemory keeps the most recent WindowSize turns raw and
// compresses older turns using rule-based pruning.
type RollingWindowMemory struct {
	WindowSize int // Number of recent turns kept raw (default: 10)
}

// NewRollingWindowMemory creates a RollingWindowMemory manager.
func NewRollingWindowMemory(windowSize int) *RollingWindowMemory {
	if windowSize <= 0 {
		windowSize = 10
	}
	return &RollingWindowMemory{WindowSize: windowSize}
}

// Process implements ChatMemoryManager.
func (m *RollingWindowMemory) Process(ctx context.Context, sessionID string, history []ChatMessage) []ChatMessage {
	if len(history) <= m.WindowSize {
		return history // Short conversation — no compression needed
	}

	// 1. Keep the most recent WindowSize turns
	recent := history[len(history)-m.WindowSize:]
	older := history[:len(history)-m.WindowSize]

	// 2. Compress older turns into a single summary message
	var olderText strings.Builder
	for _, msg := range older {
		olderText.WriteString(msg.Role)
		olderText.WriteString(": ")
		olderText.WriteString(msg.Content)
		olderText.WriteString("\n")
	}

	compressedOlder := applySemanticPruning(olderText.String())

	summaryMsg := ChatMessage{
		ID:      fmt.Sprintf("msg_summary_%s", sessionID),
		Role:    "system",
		Content: "Prior Conversation Summary:\n" + compressedOlder,
	}

	result := make([]ChatMessage, 0, len(recent)+1)
	result = append(result, summaryMsg)
	result = append(result, recent...)
	return result
}
