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
	argData, _ := json.Marshal(canonicalToolArgs(args))
	return fmt.Sprintf("%s:%s", toolName, string(argData))
}

// maxResultSeenEntries bounds resultSeen. The map is diagnostic (it decides how
// loudly to complain about a repeat), so dropping it wholesale on overflow is
// safe — the worst case is a repeat being counted from 1 again.
const maxResultSeenEntries = 4096

// ObserveResult records that `tool` returned exactly `content` in this step and
// reports how many times that exact (step, tool, content) triple has now been
// seen — 1 on the first sighting.
//
// This is the argument-proof repetition signal. Tool-call hashing (see
// InspectToolCalls) keys on name+args, so a model that holds the tool fixed and
// permutes its arguments — query=Rent / query=Utilities / query=rent,
// page_limit=20 / 50 — produces a fresh hash every time and is never caught,
// even when every response is byte-identical. Identity of the *response* is the
// one thing the model cannot vary while still re-asking the same question.
//
// Scoped per step: two DAG steps may legitimately read the same payload (step 1
// gathers, step 2 formats), and calling that a loop would be wrong.
func (s *Sieve) ObserveResult(taskID, stepID, tool, content string) int {
	if s == nil || content == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resultSeen == nil {
		s.resultSeen = make(map[string]int)
	}
	sum := sha256.Sum256([]byte(content))
	key := taskID + "|" + stepID + "|" + tool + "|" + hex.EncodeToString(sum[:16])
	if _, seen := s.resultSeen[key]; !seen && len(s.resultSeen) >= maxResultSeenEntries {
		s.resultSeen = make(map[string]int)
	}
	s.resultSeen[key]++
	return s.resultSeen[key]
}

// isReadTool returns true for tool names that are read-only (no state side
// effects). Uses a prefix heuristic on the tool's *method* name.
// Everything else is treated as a potential write → clears the read cache.
//
// 🔴 Namespace handling (fixed 2026-10-19): MCP tools are named
// "<server>__<method>" — venmo__show_transactions, supervisor__show_addresses,
// spotify__show_playlist_library. Matching the verb against the whole name
// classified EVERY namespaced tool as a write, because "venmo__show_..." starts
// with "venmo". That silently disabled two guards for the entire AppWorld
// family (10 apps, ~60 tools): the read cache never cached, and
// CheckResultRepetition never ran. Measured consequence on 4fab96f_3: 61 calls
// to venmo__show_transactions with permuted `query` arguments returning the
// same 7597-char payload, 30 identical calls to show_sent_payment_requests,
// no guard firing once, step dead at MAX_TURNS_EXHAUSTED after 40 turns.
//
// So: strip the namespace, then match the verb. Unprefixed names are unchanged.
func isReadTool(name string) bool {
	n := strings.ToLower(name)
	if i := strings.LastIndex(n, toolNamespaceSeparator); i >= 0 {
		n = n[i+len(toolNamespaceSeparator):]
	}
	for _, p := range readToolPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// toolNamespaceSeparator splits "<server>__<method>". AppWorld, tau2 and the
// built-in servers all follow it.
const toolNamespaceSeparator = "__"

// readToolPrefixes are method-name verbs that imply no state change. "show" and
// "view" cover AppWorld's API naming; "describe"/"inspect"/"retrieve"/"find"/
// "lookup"/"count" cover the tau2 and common REST-MCP conventions.
var readToolPrefixes = []string{
	"get", "list", "check", "query", "search", "read", "fetch", "is", "has",
	"show", "view", "describe", "inspect", "retrieve", "find", "lookup", "count",
}

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
