package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/store"
)

func (s *Sieve) InspectToolCalls(taskID, stepID string, calls []store.ToolInteraction) (bool, string) {
	if len(calls) == 0 {
		return true, ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Scope tool history per-step to avoid cross-step false positives.
	// toolResultCache and resultContentHistory remain per-task (cross-step
	// read caching is beneficial).
	histKey := taskID
	if stepID != "" {
		histKey = taskID + ":" + stepID
	}

	for _, call := range calls {
		argData, _ := json.Marshal(call.Arguments)
		hash := fmt.Sprintf("%s:%s", call.ToolName, string(argData))

		history := s.toolHistory[histKey]

		// Poller tools (poll/progress/health/wait) are exempt from all
		// repetition checks — they legitimately repeat with the same args
		// while waiting for external state to change.
		if !isPollerTool(call.ToolName) {
			consecutiveCount := 0
			for i := len(history) - 1; i >= 0; i-- {
				if history[i] == hash {
					consecutiveCount++
				} else {
					break
				}
			}
			if consecutiveCount >= s.ToolRepetitionThreshold {
				s.recordInterceptLocked(taskID, hash)
				return false, "InfiniteLoopDetected"
			}

			windowSize := s.ToolRepetitionThreshold * 2
			if windowSize > len(history) {
				windowSize = len(history)
			}
			windowStart := len(history) - windowSize
			totalInWindow := 0
			for i := windowStart; i < len(history); i++ {
				if history[i] == hash {
					totalInWindow++
				}
			}
			if totalInWindow >= s.ToolRepetitionThreshold && windowSize > 0 {
				if float64(totalInWindow)/float64(windowSize) >= 0.5 {
					s.recordInterceptLocked(taskID, hash)
					return false, "InfiniteLoopDetected"
				}
			}

			// Total-occurrence check: catches sparse interleaved repetition
			// (same tool+args called many times across a session with other
			// calls between) that the consecutive and window checks miss.
			// Conservative 2× multiplier avoids false-positiveing legitimate
			// retries (allows up to 2×threshold-1 total calls).
			// Note: scoped per-step via histKey — cross-step repeats are NOT
			// counted (different steps may legitimately call the same tool).
			totalCount := 0
			for i := 0; i < len(history); i++ {
				if history[i] == hash {
					totalCount++
				}
			}
			if totalCount >= s.ToolRepetitionThreshold*2 {
				s.recordInterceptLocked(taskID, hash)
				return false, "InfiniteLoopDetected"
			}
		}

		s.toolHistory[histKey] = append(s.toolHistory[histKey], hash)
		if len(s.toolHistory[histKey]) > maxToolHistoryPerTask {
			s.toolHistory[histKey] = s.toolHistory[histKey][len(s.toolHistory[histKey])-maxToolHistoryPerTask:]
		}

		// ABAB cycle detection (Gap-1): catches residual narrow case where
		// period 2-4 cycles slip through consecutive/window/total checks.
		// e.g., A,B,C,D,A,B,C,D,A,B,C,D — each hash appears only 3 times
		// total (< threshold*2=6), and < 3 in any 6-window (< 50% density).
		if !isPollerTool(call.ToolName) && detectCycle(s.toolHistory[histKey], s.ToolRepetitionThreshold) {
			s.recordInterceptLocked(taskID, hash)
			return false, "InfiniteLoopDetected"
		}
	}

	// Cap the number of tracked tasks to prevent unbounded map growth.
	// Clearing is safe: old completed tasks are unlikely to still be
	// making tool calls, and ClearHistory is called on task completion.
	if len(s.toolHistory) > maxToolHistoryTasks {
		current := s.toolHistory[histKey]
		// audit P-MED-8: partial eviction instead of full clear to prevent cache stampede
		i := 0
		for k := range s.toolHistory {
			i++
			if i%4 == 0 {
				delete(s.toolHistory, k)
			}
		}
		s.toolHistory[histKey] = current
	}

	return true, ""
}

// detectCycle checks if the history ends with a repeating cycle of period 4.
// Returns true if a cycle is detected with at least minRepeats repetitions.
//
// This catches the residual narrow case (Gap-1) that consecutive, window,
// and total-occurrence checks all miss: cycles with period ≥ 4 where each
// individual hash appears < threshold times in any window and < threshold*2
// total, yet the overall pattern is clearly repetitive.
//
// Period 2 and 3 are NOT checked here because they are already covered by
// the window check (50% density) and total-occurrence check (totalCount ≥ 6)
// respectively. Only period 4 is the residual narrow case.
//
// Example: A,B,C,D,A,B,C,D,A,B,C,D (period 4, 3 repeats) — each hash
// appears only 3 times total (< 6) and ≤ 1 time in any 6-window (< 50%).
//
// Inspired by Hermes Agent `_detect_identical_cycle()` (64-entry deque,
// period 2-4 detection).
func detectCycle(history []string, minRepeats int) bool {
	n := len(history)
	p := 4 // only period 4 — periods 2-3 already covered by existing checks
	if n < p*minRepeats {
		return false
	}
	// Check if the last p*minRepeats entries form a cycle of period p.
	pattern := history[n-p:]
	for r := 1; r < minRepeats; r++ {
		start := n - p*(r+1)
		for j := 0; j < p; j++ {
			if history[start+j] != pattern[j] {
				return false
			}
		}
	}
	return true
}

// InspectTruncationArtifact checks if the model is over-generalizing from
// truncated tool results. When the last tool's Meta.Truncated is true and
// the model output contains universal quantifiers (all/every/complete/etc., incl. CJK 全部/所有),
// the Sieve blocks the output and forces the model to narrow its conclusion.
//
// See docs/plan/STRUCTURED_TOOL_METADATA_DESIGN.md §2.2.
func (s *Sieve) InspectTruncationArtifact(lastMeta *store.ToolMetadata, modelOutput string) (bool, string) {
	if lastMeta == nil || !lastMeta.Truncated {
		return true, ""
	}

	forbiddenTerms := []string{"全部", "所有", "完整", "全体", "all", "entire", "complete"}
	lowerOutput := strings.ToLower(modelOutput)
	for _, term := range forbiddenTerms {
		if strings.Contains(lowerOutput, term) {
			hint := lastMeta.Hint
			if hint == "" {
				hint = "Previous tool output was truncated. Do not claim comprehensive coverage."
			}
			return false, fmt.Sprintf("TruncationViolation: Output contains '%s' but tool result was truncated (returned %d of %d). Hint: %s", term, lastMeta.Returned, lastMeta.Total, hint)
		}
	}
	return true, ""
}

func (s *Sieve) ClearHistory(taskID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.toolHistory, taskID)
	// Also clear all step-scoped histories (keys "taskID:stepID").
	// This prevents unbounded growth when a task has many steps.
	prefix := taskID + ":"
	for k := range s.toolHistory {
		if strings.HasPrefix(k, prefix) {
			delete(s.toolHistory, k)
		}
	}
	delete(s.toolResultCache, taskID)
	delete(s.resultContentHistory, taskID)
	// resultSeen keys are "<taskID>|<stepID>|<tool>|<hash>".
	seenPrefix := taskID + "|"
	for k := range s.resultSeen {
		if strings.HasPrefix(k, seenPrefix) {
			delete(s.resultSeen, k)
		}
	}
	delete(s.plans, taskID) // also clear step plan (#4)

	// Adaptive threshold relaxation/tightening (#13): evaluate the completed
	// task's intercept stats and adjust the global threshold.
	stats := s.interceptStats[taskID]
	if stats == nil || stats.totalIntercepts == 0 {
		// Clean task — increment consecutive counter for relaxation.
		s.consecutiveCleanTasks++
		if s.consecutiveCleanTasks >= adaptiveRelaxConsecutive && s.ToolRepetitionThreshold > adaptiveThresholdMin {
			s.ToolRepetitionThreshold--
			s.consecutiveCleanTasks = 0
		}
	} else {
		// Intercepting task — reset clean counter.
		s.consecutiveCleanTasks = 0
		// Tighten if ≥2 distinct hashes (deferred from recordInterceptLocked
		// to avoid changing behavior mid-task).
		if stats.distinctIntercepts >= 2 && s.ToolRepetitionThreshold < adaptiveThresholdMax {
			s.ToolRepetitionThreshold++
		}
	}
	delete(s.interceptStats, taskID)
}

// recordInterceptLocked records an interception for adaptive threshold
// tuning. Must be called with s.mu held. When a task accumulates ≥2
// distinct intercepted hashes, the model is "varying the pattern but
// still looping" → mark the task for threshold tightening.
//
// The actual threshold change is deferred to ClearHistory (task completion)
// to avoid changing loop detection behavior mid-task — tightening within
// the same task could cause the current task's loops to go undetected.
func (s *Sieve) recordInterceptLocked(taskID, hash string) {
	if s.interceptStats == nil {
		s.interceptStats = make(map[string]*interceptStat)
	}
	stats := s.interceptStats[taskID]
	if stats == nil {
		stats = &interceptStat{interceptedHashes: make(map[string]bool)}
		s.interceptStats[taskID] = stats
	}
	stats.totalIntercepts++
	if !stats.interceptedHashes[hash] {
		stats.interceptedHashes[hash] = true
		stats.distinctIntercepts++
	}
}

// GetCachedResult returns a previously cached result for the exact same
// tool+args, if one exists. Used for cache replay instead of force-terminate.
