package core

import (
	"github.com/daybeam/vortex/pkg/observability"
	"sync"
)

// UsageStats tracks token consumption across different models.
type UsageStats struct {
	Mu               sync.RWMutex
	TotalTokens      map[string]int64 `json:"total_tokens"`
	PromptTokens     map[string]int64 `json:"prompt_tokens"`
	CompletionTokens map[string]int64 `json:"completion_tokens"`
	CacheWriteTokens map[string]int64 `json:"cache_write_tokens"`
	CacheReadTokens  map[string]int64 `json:"cache_read_tokens"`
	ToolCalls        map[string]int64 `json:"tool_calls"`
}

var globalStats = &UsageStats{
	TotalTokens:      make(map[string]int64),
	PromptTokens:     make(map[string]int64),
	CompletionTokens: make(map[string]int64),
	CacheWriteTokens: make(map[string]int64),
	CacheReadTokens:  make(map[string]int64),
	ToolCalls:        make(map[string]int64),
}

func GetGlobalStats() *UsageStats {
	return globalStats
}

func (s *UsageStats) RecordUsage(provider, model string, prompt, completion int, toolCalls int, cacheWrite, cacheRead int) {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	// Track by model
	s.PromptTokens[model] += int64(prompt)
	s.CompletionTokens[model] += int64(completion)
	s.CacheWriteTokens[model] += int64(cacheWrite)
	s.CacheReadTokens[model] += int64(cacheRead)
	s.TotalTokens[model] += int64(prompt + completion)
	s.ToolCalls[model] += int64(toolCalls)

	// Track by provider if different from model
	if provider != "" && provider != model {
		s.PromptTokens[provider] += int64(prompt)
		s.CompletionTokens[provider] += int64(completion)
		s.CacheWriteTokens[provider] += int64(cacheWrite)
		s.CacheReadTokens[provider] += int64(cacheRead)
		s.TotalTokens[provider] += int64(prompt + completion)
		s.ToolCalls[provider] += int64(toolCalls)
	}
}

func (s *UsageStats) GetSnapshot() map[string]any {
	s.Mu.RLock()
	defer s.Mu.RUnlock()

	snapshot := make(map[string]any)
	for model, total := range s.TotalTokens {
		snapshot[model] = map[string]any{
			"total_tokens":      total,
			"prompt_tokens":     s.PromptTokens[model],
			"completion_tokens": s.CompletionTokens[model],
			"cache_write":       s.CacheWriteTokens[model],
			"cache_read":        s.CacheReadTokens[model],
			"tool_calls":        s.ToolCalls[model],
		}
	}

	// Add observability metrics (ADDED 2026-08-28)
	metrics := observability.GetGlobalMetrics().GetSnapshot()
	for k, v := range metrics {
		snapshot["_observability_"+k] = v
	}

	return snapshot
}
