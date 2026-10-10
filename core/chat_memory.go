package core

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ChatMemoryManager transforms raw chat history into a bounded-context history
// before it is sent to the provider. Implementations may apply sliding
// windows, rolling summaries, fact extraction, and token-budget trimming.
type ChatMemoryManager interface {
	Process(ctx context.Context, sessionID string, history []ChatMessage) []ChatMessage
}

// RollingWindowMemory keeps the most recent WindowSize turns raw and
// compresses older turns. It supports three optional layers:
//   - Layer 2: LLM-based summarization (SummaryModel) — falls back to rule-based
//   - Layer 3: Fact extraction (FactCache) — durable facts injected as system context
//   - Token-budget trimming (MaxTokens) — drops oldest messages when over budget
type RollingWindowMemory struct {
	WindowSize   int                // recent N turns kept raw (default: 10)
	SummaryModel providers.Provider  // optional LLM for summarization (nil = rule-based)
	FactCache    *FactCache          // optional fact extraction (nil = skip)
	MaxTokens    int                // optional token budget (0 = no limit)
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
	var facts string
	if m.FactCache != nil {
		facts = m.FactCache.ExtractAndInject(history)
	}

	if len(history) <= m.WindowSize {
		result := make([]ChatMessage, 0, len(history)+1)
		if facts != "" {
			result = append(result, ChatMessage{Role: "system", Content: facts})
		}
		result = append(result, history...)
		return m.enforceTokenBudget(result)
	}

	recent := history[len(history)-m.WindowSize:]
	older := history[:len(history)-m.WindowSize]

	summary := m.summarize(ctx, older)

	result := make([]ChatMessage, 0, len(recent)+2)
	result = append(result, ChatMessage{
		ID:      fmt.Sprintf("msg_summary_%s", sessionID),
		Role:    "system",
		Content: "Prior Conversation Summary:\n" + summary,
	})
	if facts != "" {
		result = append(result, ChatMessage{Role: "system", Content: facts})
	}
	result = append(result, recent...)

	return m.enforceTokenBudget(result)
}

func (m *RollingWindowMemory) summarize(ctx context.Context, older []ChatMessage) string {
	var olderText strings.Builder
	for _, msg := range older {
		olderText.WriteString(msg.Role)
		olderText.WriteString(": ")
		olderText.WriteString(msg.Content)
		olderText.WriteString("\n")
	}
	text := olderText.String()

	if m.SummaryModel == nil {
		return applySemanticPruning(text)
	}

	resp, err := m.SummaryModel.Complete(ctx, schemas.CompleteRequest{
		System: SysPromptChatSummary,
		User:   text,
	})
	if err != nil || resp == nil || resp.Text == "" {
		return applySemanticPruning(text)
	}
	return resp.Text
}

func (m *RollingWindowMemory) enforceTokenBudget(msgs []ChatMessage) []ChatMessage {
	if m.MaxTokens <= 0 || m.estimateTokens(msgs) <= m.MaxTokens {
		return msgs
	}
	for len(msgs) > 1 && m.estimateTokens(msgs) > m.MaxTokens {
		msgs = append(msgs[:1], msgs[2:]...)
	}
	return msgs
}

func (m *RollingWindowMemory) estimateTokens(msgs []ChatMessage) int {
	total := 0
	for _, msg := range msgs {
		total += len(msg.Content) / 4
	}
	return total
}

// FactCache extracts durable facts from chat history using simple rules
// (file paths, language preferences) and optionally persists them to
// MemoryBankStore.UserProfile.Preferences.
type FactCache struct {
	Store *store.MemoryBankStore
}

var (
	factPathRe   = regexp.MustCompile(`(?:/[a-zA-Z0-9_./-]{3,}|[A-Za-z]:\\[a-zA-Z0-9_\\.-]{3,})`)
	factPreferRe = regexp.MustCompile(`(?i)(?:I (?:prefer|use|like|need)|using)\s+([a-zA-Z0-9_-]{2,20})`)
)

func (fc *FactCache) ExtractAndInject(history []ChatMessage) string {
	seen := make(map[string]bool)
	var facts []string

	for _, msg := range history {
		if msg.Role != "user" {
			continue
		}
		for _, match := range factPathRe.FindAllString(msg.Content, -1) {
			if !seen[match] {
				seen[match] = true
				facts = append(facts, "path: "+match)
			}
		}
		for _, match := range factPreferRe.FindAllStringSubmatch(msg.Content, -1) {
			fact := "prefers " + match[1]
			if !seen[fact] {
				seen[fact] = true
				facts = append(facts, fact)
			}
		}
	}

	if len(facts) == 0 {
		return ""
	}

	if fc.Store != nil {
		if mb, err := fc.Store.Load(); err == nil {
			merged := mergeUnique(mb.UserProfile.Preferences, facts)
			up := mb.UserProfile
			up.Preferences = merged
			if err := fc.Store.SaveUserProfile(up); err != nil {
				log.Printf("WARN: chat_memory: failed to save user profile: %v", err)
			}
		}
	}

	return "Durable facts:\n" + strings.Join(facts, "\n")
}

func mergeUnique(existing, additions []string) []string {
	seen := make(map[string]bool)
	for _, s := range existing {
		seen[s] = true
	}
	result := make([]string, 0, len(existing)+len(additions))
	result = append(result, existing...)
	for _, s := range additions {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
