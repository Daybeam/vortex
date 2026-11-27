package chat

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// runStats holds the per-run counters that only [chat-turn-summary] reads.
// They used to be locals in Run; the extraction keeps Run inside its Q6-1
// ratchet (harness_size_ratchet_test.go) while the M-step metrics are added.
// Same shape and same reason as exploreGuard (code_exec_guard.go).
type runStats struct {
	start        time.Time
	turns        int
	toolCalls    int
	codeExec     int
	noTextSteers int
	exitReason   string // "" ⇒ derive from err; "done" / "max_turns" set explicitly

	contractDeclared int
	fieldsDeclared   int

	toolCounts   map[string]int
	offeredTools int
}

func newRunStats(contractDeclared, fieldsDeclared int) *runStats {
	return &runStats{
		start:            time.Now(),
		contractDeclared: contractDeclared,
		fieldsDeclared:   fieldsDeclared,
		toolCounts:       make(map[string]int),
	}
}

// recordTool counts one model-requested call, including calls served from the
// sieve cache: the histogram answers what the model asked for, not what
// reached a tool. execute_code is counted separately because its writes are
// invisible to writeTargetOf (writeToolArgKeys has no "code") — that gap is
// why p22 recorded writes=1 next to ~70 rewrites of the same CSV.
func (s *runStats) recordTool(name string) {
	s.toolCalls++
	s.toolCounts[name]++
	if name == "execute_code" {
		s.codeExec++
	}
}

// offerTools returns this turn's tool table and records its size. Only the
// first turn is kept: the hard post-delegation lockout removes the work tools
// mid-run (chat_tool_defs.go), and the denominator for offered/called is the
// surface the run started with. The shrink itself is already reported by
// post_deleg= and delegated=.
func (s *runStats) offerTools(h *ChatHarness) []schemas.ToolDefinition {
	defs := h.ToolDefinitions()
	if s.offeredTools == 0 {
		s.offeredTools = len(defs)
	}
	return defs
}

// histogram renders the called tools as a sorted name=count list so two runs
// can be diffed byte-for-byte. "na" matches the contract_* convention on the
// same line and keeps the field present on a run that called nothing.
func (s *runStats) histogram() string {
	if len(s.toolCounts) == 0 {
		return "na"
	}
	names := make([]string, 0, len(s.toolCounts))
	for name := range s.toolCounts {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, s.toolCounts[name]))
	}
	return strings.Join(parts, ",")
}

// emitSummary writes the one structured line per run. Called from a defer so
// a new early return can never silently skip the measurement. Existing keys
// keep their names and order; new state rides on the end of the line.
func (s *runStats) emitSummary(h *ChatHarness, taskID string, err error, eg *exploreGuard, contractSteers, contractDefectSteers int) {
	reason := s.exitReason
	if reason == "" {
		if err != nil {
			reason = "error"
		} else {
			reason = "done"
		}
	}
	log.Printf("[chat-turn-summary] task=%s exit=%s turns=%d tool_calls=%d code_exec=%d "+
		"writes=%d distinct_targets=%d steer_no_text=%d repeated_write=%d contract_declared=%d contract_steer=%d "+
		"contract_defect_steer=%d fields_declared=%d explore_streak_max=%d explore_steer=%d explore_level=%d post_deleg=%s post_deleg_blocked=%d calls_after_deleg=%d delegated=%d post_deleg_polls=%d post_deleg_poll_blocked=%d fastpath_hit=%v elapsed=%.1fs %s tool_histogram=%s offered_tools=%d %s",
		taskID, reason, s.turns, s.toolCalls, s.codeExec, eg.writeTotal,
		len(eg.writeTargets), s.noTextSteers, eg.repeatedWriteSteers, s.contractDeclared, contractSteers,
		contractDefectSteers, s.fieldsDeclared, eg.streakMax, eg.steers, eg.level, h.pd.arm, h.pd.blocked, h.pd.after, h.pd.delegations, h.pd.polls, h.pd.pollBlocked, h.pd.fastpathHit,
		time.Since(s.start).Seconds(), h.contractObservation(), s.histogram(), s.offeredTools, s.budgetObservation(h, err))
}

// turns is the harness loop counter, not a provider-request ledger.
func (s *runStats) budgetObservation(h *ChatHarness, err error) string {
	effective := h.MaxTurns
	if effective <= 0 {
		effective = DefaultMaxTurns
	}
	remaining := effective - s.turns
	if remaining < 0 {
		remaining = 0
	}
	stopReason := "none"
	var budget *providers.BudgetExhaustedError
	if errors.As(err, &budget) {
		stopReason = "provider_budget"
	}
	return fmt.Sprintf("cap_scope=harness_turns requested_cap=%d effective_cap=%d used_turns=%d remaining=%d %s budget_stop_reason=%s",
		h.MaxTurns, effective, s.turns, remaining, delegationETAObservation(h), stopReason)
}

func delegationETAObservation(h *ChatHarness) string {
	if h.pd.taskID == "" || h.DelegateStatus == nil {
		return "eta_task=na eta_seconds=na eta_basis=unknown watchdog_delay_seconds=na"
	}
	st, ok := h.DelegateStatus(h.pd.taskID, 0)
	if !ok || st == nil {
		return fmt.Sprintf("eta_task=%s eta_seconds=na eta_basis=unknown watchdog_delay_seconds=na", h.pd.taskID)
	}
	eta := EstimateETA(st, time.Now())
	if !eta.Known() {
		return fmt.Sprintf("eta_task=%s eta_seconds=na eta_basis=%s watchdog_delay_seconds=na", h.pd.taskID, eta.Basis)
	}
	return fmt.Sprintf("eta_task=%s eta_seconds=%.1f eta_basis=%s watchdog_delay_seconds=%.1f",
		h.pd.taskID, eta.Remaining.Seconds(), eta.Basis, eta.WatchdogDelay().Seconds())
}
