package core

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"

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
		// audit L-MED-1: log fallback for observability — previously silently
		// swallowed, making summary model failures invisible.
		if err != nil {
			log.Printf("WARN: chat_memory: summary model failed, using semantic pruning: %v", err)
		} else if resp == nil || resp.Text == "" {
			log.Printf("WARN: chat_memory: summary model returned empty response, using semantic pruning")
		}
		return applySemanticPruning(text)
	}
	return resp.Text
}

func (m *RollingWindowMemory) enforceTokenBudget(msgs []ChatMessage) []ChatMessage {
	if m.MaxTokens <= 0 || len(msgs) <= 1 {
		return msgs
	}
	total := m.estimateTokens(msgs)
	if total <= m.MaxTokens {
		return msgs
	}
	// Layout: [summary, facts?, conversation...]
	// Preserve summary (idx 0) and facts (idx 1) as long as possible.
	// Drop oldest conversation messages first, then facts, then summary.
	// O(n): single pass from newest to oldest to find what fits.
	// (audit P-CRIT-1: was O(n²) + dropped facts before conversation msgs)
	summaryTokens := len(msgs[0].Content) / 4
	convStart := 1 // first conversation message index (after summary)
	fixedTokens := summaryTokens
	if len(msgs) > 1 {
		fixedTokens += len(msgs[1].Content) / 4 // include facts
		convStart = 2
	}
	// Try keeping summary + facts + newest conversation messages.
	budget := m.MaxTokens - fixedTokens
	if budget < 0 && len(msgs) > 1 {
		// Drop facts — try summary + newest conversation only.
		fixedTokens = summaryTokens
		convStart = 1
		budget = m.MaxTokens - fixedTokens
	}
	if budget < 0 {
		return msgs[:1] // only summary fits
	}
	// Keep newest conversation messages that fit.
	keep := 0
	for i := len(msgs) - 1; i >= convStart; i-- {
		t := len(msgs[i].Content) / 4
		if t > budget {
			break
		}
		budget -= t
		keep++
	}
	// Fix: If the newest message alone exceeds budget, don't silently drop it.
	// Truncate with a marker instead so the model sees something useful.
	if keep == 0 && len(msgs) > convStart {
		newest := msgs[len(msgs)-1]
		maxChars := budget * 4 // rough token→char conversion
		if maxChars < 200 {
			maxChars = 200 // minimum to be useful
		}
		if len(newest.Content) > maxChars {
			half := maxChars / 2
			omitted := len(newest.Content) - maxChars
			newest.Content = newest.Content[:half] +
				fmt.Sprintf("\n[... message truncated: %d chars omitted to fit token budget ...]\n", omitted) +
				newest.Content[len(newest.Content)-half:]
		}
		result := make([]ChatMessage, 0, convStart+1)
		result = append(result, msgs[:convStart]...)
		result = append(result, newest)
		return result
	}
	result := make([]ChatMessage, 0, convStart+keep)
	result = append(result, msgs[:convStart]...)
	result = append(result, msgs[len(msgs)-keep:]...)
	return result
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
	// audit C-HIGH-5: serializes Load+merge+Save to prevent lost updates
	// when multiple goroutines extract facts concurrently.
	mu sync.Mutex
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
		fc.mu.Lock()
		defer fc.mu.Unlock()
		if mb, err := fc.Store.Load(); err == nil {
			merged := mergeUnique(mb.UserProfile.Preferences, facts)
			up := mb.UserProfile
			up.Preferences = merged
			if err := fc.Store.SaveUserProfile(up); err != nil {
				log.Printf("WARN: chat_memory: failed to save user profile: %v", err)
			}
		} else {
			// audit L-LOW-1: log Load failure — previously silently ignored,
			// making fact persistence failures invisible.
			log.Printf("WARN: chat_memory: failed to load memory bank for fact injection: %v", err)
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
