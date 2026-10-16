package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

func (s *Sieve) GetCachedResult(taskID, toolName string, args map[string]any) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := toolCallHash(toolName, args)
	if cache, ok := s.toolResultCache[taskID]; ok {
		if result, ok := cache[hash]; ok {
			return result, true
		}
	}
	return "", false
}

// CacheToolResult stores a successful read-tool result for future replay.
func (s *Sieve) CacheToolResult(taskID, toolName string, args map[string]any, result string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := toolCallHash(toolName, args)
	if s.toolResultCache[taskID] == nil {
		s.toolResultCache[taskID] = make(map[string]string)
	}
	// #16: Cache backpressure — when per-task cache exceeds maxCachePerTask,
	// clear the entire cache for this task (drop-oldest semantics: the
	// oldest entries are from earlier turns; if the model needs them,
	// it's already lost). This prevents unbounded memory growth.
	if len(s.toolResultCache[taskID]) >= maxCachePerTask {
		s.toolResultCache[taskID] = make(map[string]string)
	}
	s.toolResultCache[taskID][hash] = result
}

// ClearResultCache clears all cached results for a task (call after writes).
func (s *Sieve) ClearResultCache(taskID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.toolResultCache, taskID)
}

// CheckResultRepetition records a result's content hash and returns true
// if the same result content has been seen >= ToolRepetitionThreshold times
// in this task's history. This is a post-execution check that catches
// cases where different tool calls (e.g., different entity ID casings)
// return identical results — a universal stuck signal that the call-level
// hash check misses because the args differ.
//
// Inspired by OpenHands StuckDetector's content-based event equality.
// See docs/HARNESS_LOOP_GUARD_RESEARCH.md §7.
func (s *Sieve) CheckResultRepetition(taskID, result string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	h := sha256.Sum256([]byte(result))
	rHash := hex.EncodeToString(h[:])

	history := s.resultContentHistory[taskID]
	count := 0
	for _, prev := range history {
		if prev == rHash {
			count++
		}
	}

	s.resultContentHistory[taskID] = append(history, rHash)
	if len(s.resultContentHistory[taskID]) > maxToolHistoryPerTask {
		s.resultContentHistory[taskID] = s.resultContentHistory[taskID][len(s.resultContentHistory[taskID])-maxToolHistoryPerTask:]
	}

	return count >= s.ToolRepetitionThreshold
}

func toolCallHash(toolName string, args map[string]any) string {
	argData, _ := json.Marshal(args)
	return fmt.Sprintf("%s:%s", toolName, string(argData))
}

// isReadTool returns true for tool names that are read-only (no state side
// effects). Uses a prefix heuristic: get/list/check/query/search/read/fetch.
// Everything else is treated as a potential write → clears the read cache.
func isReadTool(name string) bool {
	n := strings.ToLower(name)
	for _, p := range readToolPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

var readToolPrefixes = []string{"get", "list", "check", "query", "search", "read", "fetch", "is", "has"}

// isPollerTool returns true for tools that return time-varying results with
// the same args (polling, progress, health). These are exempt from loop
// detection and never cached — they must always execute fresh.
func isPollerTool(name string) bool {
	n := strings.ToLower(name)
	for _, s := range pollerSignals {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

var pollerSignals = []string{"poll", "progress", "health", "wait"}

// SetSystemOneProvider injects the System One decision model for the Sieve's
// Noul/Score safety check (M5). Nil-safe: when provider is nil or EnableSieve
