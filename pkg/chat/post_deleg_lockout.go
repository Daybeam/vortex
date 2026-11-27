package chat

import (
	"fmt"
	"log"
	"os"

	"strings"

	"github.com/daybeam/vortex/schemas"
)

// Post-delegation tool lockout — the lever asked for by §9.11 of
// docs/completed/2026-11-24/DELEGATION_INTENT_TRIGGER_ANALYSIS.md.
//
// Measured (235 delegating runs, harness-bench): 82% delegate as their FIRST
// action, then make a median of 11 more tool calls; execute_code alone accounts
// for 2179 calls after the receipt. Only 1% delegate and then stop. Total calls
// are 9.4 with delegation vs 9.5 without, and scores do not move (p=1.0 across
// three separate pairings). So delegation is not "work moved off the
// foreground", it is "send a receipt, then keep doing the work" — and no
// prompt-level change can fix that, because the tool table still offers every
// work tool.
//
// This file is the mechanism: once a task has been delegated in a run, the work
// tools stop being usable for the rest of that run. Two arms:
//
//	soft  the tool stays in the table but returns a lockout message
//	hard  the tool is removed from the table and stays removed
//
// Off by default: with VORTEX_POST_DELEG_LOCKOUT unset, behaviour is exactly
// what it was before this file existed. Everything here exists in this file
// rather than in Run because Q6-1's ratchet (harness_size_ratchet_test.go)
// forbids growing Run.
const (
	PostDelegOff  = ""
	PostDelegSoft = "soft"
	PostDelegHard = "hard"
)

// postDelegWorkTools are the tools that let the model keep PRODUCING the
// delegated task itself. Deliberately NOT included:
//
//	read_file                — read-only, so it cannot duplicate the deliverable
//	orchestrator_task_status — the way to collect the result
//	orchestrator_help        — read-only manual
//	search_archive           — read-only recall, cannot produce a deliverable
//	delegate_to_orchestrator — re-delegating a sub-part is legitimate
//
// read_file was in this map first and had to come out. Measured over 240
// delegating runs (_post_deleg_kind.py): 722 post-delegation read_file calls,
// of which 0 produce anything — it is 100% inspection. Blocking it therefore
// bought no reduction in duplicated work and removed the only way the parent
// could check what the sub-task wrote. The one run that did block it (223,
// soft, n=1) spent 8 refused read_file calls, made zero write attempts, and
// stopped with three executable checks failing — a parent that cannot read the
// artifacts cannot catch the sub-task's mistakes, and the run becomes
// "delegate and trust". execute_code is the lever: 2282 post-delegation calls,
// 81% of them containing a write.
var postDelegWorkTools = map[string]bool{
	"execute_code":  true,
	"write_file":    true,
	"batch_replace": true,
}

// postDelegState is the per-run lockout state. The zero value is "not resolved
// yet", so a harness that never touches the env behaves exactly as before.
type postDelegState struct {
	resolved bool
	arm      string // postDelegOff | soft | hard
	taskID   string // most recent hand-off (log lines, messages, hard-arm filter)
	blocked  int    // tool calls intercepted after delegating (attempts refused)
	after    int    // tool calls that actually RAN after delegating (work left in the foreground)

	// taskIDs is every task this SESSION handed off, not just the last one.
	// Keying the poll lever on a single id lost the guard whenever a turn
	// delegated twice: measured 2026-11-11, `delegated=2`, two
	// orchestrator_task_status calls in the trajectory, `post_deleg_polls=1` --
	// the check for the first task was neither counted nor blocked. It is also
	// what a wake turn is seeded with (SeedDelegatedTasks): that turn is a new
	// harness, and the only status call of the measured run landed there.
	taskIDs map[string]struct{}

	// delegatedHere is "this harness delegated", which is NOT the same question
	// as "this session has handed something off". The lockout and the
	// calls_after_deleg baseline are per-turn (a wake turn must be able to read
	// the result and write its artifacts); the poll lever is per-session.
	delegatedHere bool

	// Polling lever (§9.12.5). VORTEX_POST_DELEG_POLL=wake answers the first
	// status check for a delegated task immediately (wait=0) and locks every
	// later one, so the model is told to stop and wait for the wake instead of
	// burning the foreground budget on polls -- measured at ~3.3 polls per run
	// (§9.12.2). Off by default: a wake that is lost means the result is never
	// read, so this arm is opt-in until [chat-wake] is proven in production.
	pollResolved bool
	pollWake     bool
	delegations  int // successful delegate_to_orchestrator calls made BY THIS harness
	polls        int // status checks that ran for a delegated task
	pollBlocked  int // status checks refused after the first
	fastpathHit  bool // at least one delegated task took the single-step inline branch
}

// armMode resolves the arm lazily, once per harness. Lazy so Run needs no init
// line (Q6-1: Run may only shrink).
func (p *postDelegState) armMode() string {
	if !p.resolved {
		p.arm = postDelegModeFromEnv()
		p.resolved = true
		if p.arm != PostDelegOff {
			log.Printf("[post-deleg] arm=%s", p.arm)
		}
	}
	return p.arm
}

// postDelegModeFromEnv resolves the arm from VORTEX_POST_DELEG_LOCKOUT.
// Unknown values are treated as off rather than as an error: a typo in an env
// var must not change behaviour, it must leave it alone — and the banner above
// makes the typo visible in the log, which an error would not.
func postDelegModeFromEnv() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VORTEX_POST_DELEG_LOCKOUT"))) {
	case "soft", "1", "true", "on":
		return PostDelegSoft
	case "hard":
		return PostDelegHard
	default:
		return PostDelegOff
	}
}

// PostDelegPollEnv selects the polling lever: unset = off, wake/1/true/on =
// one immediate check and then locked. Unknown values stay off (same rule as
// the arm: a typo must not change behaviour).
const PostDelegPollEnv = "VORTEX_POST_DELEG_POLL"

// pollWakeMode resolves the polling lever lazily, once per harness.
func (p *postDelegState) pollWakeMode() bool {
	if !p.pollResolved {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(PostDelegPollEnv))) {
		case "wake", "1", "true", "on":
			p.pollWake = true
		}
		p.pollResolved = true
		if p.pollWake {
			log.Printf("[post-deleg] poll=wake (first status check is immediate, later ones locked)")
		}
	}
	return p.pollWake
}

// recordDelegation remembers a task this harness handed off. One place, because
// three things move together: the set the poll lever matches against, the last
// id the messages quote, and the per-turn flag the lockout reads.
func (p *postDelegState) recordDelegation(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if p.taskIDs == nil {
		p.taskIDs = make(map[string]struct{}, 2)
	}
	p.taskIDs[id] = struct{}{}
	p.taskID = id
	p.delegatedHere = true
	p.delegations++
}

// seedDelegatedTasks carries the hand-offs of earlier turns into this harness.
// It deliberately does NOT set delegatedHere and does not count as a
// delegation: a wake turn has to be able to read the result and write its
// artifacts, so the lockout must stay out of it (soft/hard are per-turn
// levers). Only the poll lever is cross-turn.
func (p *postDelegState) seedDelegatedTasks(ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	// Fresh map, never the caller's: harness clones share pointer fields, and a
	// shared map here would be a data race between concurrent turns.
	next := make(map[string]struct{}, len(ids)+len(p.taskIDs))
	for k := range p.taskIDs {
		next[k] = struct{}{}
	}
	added := 0
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := next[id]; !ok {
			added++
		}
		next[id] = struct{}{}
		if p.taskID == "" {
			p.taskID = id
		}
	}
	p.taskIDs = next
	return added
}

// wasDelegated reports whether id is a task this session handed off.
func (p *postDelegState) wasDelegated(id string) bool {
	_, ok := p.taskIDs[strings.TrimSpace(id)]
	return ok
}

// isWakePoll reports whether this status call is the one free check the wake
// mode allows. execTool forces wait=0 for it, so the check cannot block either.
func (p *postDelegState) isWakePoll(id string) bool {
	return p.pollWakeMode() && p.wasDelegated(id) && p.polls == 0
}

// blocksPoll reports whether a status call for a delegated task must be
// refused, and counts the refusal. A poll for any other task is left alone:
// the lever is about the tasks this session handed off, not about status in
// general.
func (p *postDelegState) blocksPoll(name string, args map[string]any) bool {
	if !p.pollWakeMode() || len(p.taskIDs) == 0 || name != "orchestrator_task_status" {
		return false
	}
	id, _ := args["task_id"].(string)
	if !p.wasDelegated(id) {
		return false
	}
	if p.polls == 0 {
		return false // the immediate check; see isWakePoll
	}
	p.pollBlocked++
	return true
}

// notePoll counts a status check that ran for a delegated task. Counted on
// every arm, so the off baseline and the wake arm come from the same binary.
func (p *postDelegState) notePoll(id string) {
	if p.wasDelegated(id) {
		p.polls++
	}
}

// postDelegWakeNote is appended to the one immediate status answer when the
// task is still running. It has to say three things: the check it just got is
// all it gets, the wake is automatic, and the turn should end now.
const postDelegWakeNote = "still running: this session is resumed automatically when the task finishes, " +
	"and the result is delivered then. Do not call orchestrator_task_status again for this task — " +
	"reply to the user with the state you have and stop this turn."

// postDelegPollBlockedMessage answers a second poll. Named and actionable for
// the same reason as the work-tool lockout: a bare denial makes the model
// retry the same call.
func postDelegPollBlockedMessage(taskID string) string {
	return fmt.Sprintf(
		"[orchestrator_task_status IS LOCKED for this run] %s was delegated and the first status "+
			"check already ran. The task is still in progress; this session is resumed "+
			"automatically when it finishes, and the result is delivered then. Polling again cannot "+
			"make it finish sooner. Reply to the user with the state you have and stop this turn.\n",
		taskID)
}

// postDelegLockoutMessage is what a locked-out tool returns. It names the task
// id and the only two moves left, because a lockout without a next step just
// makes the model retry the same tool (iron law 181: the fix belongs at the
// step that broke, not in a generic warning).
func postDelegLockoutMessage(tool, taskID string) string {
	return fmt.Sprintf(
		"[TOOL %s IS LOCKED for this run] This task was already delegated to the orchestrator as %s; "+
			"the work happens in that task now. Do not try to produce its deliverables here.\n"+
			"Available: orchestrator_task_status(task_id=%q, wait_seconds=20) to collect the result "+
			"(call it at most a couple of times), read_file to inspect what the task produced — reading "+
			"is allowed, writing is not — or reply to the user with the task id now: this session is "+
			"resumed automatically when the task finishes.\n",
		tool, taskID, taskID)
}

// beforeToolCall runs every per-call guard that must see the model's intent
// before execTool does, and reports whether the call must be skipped.
//
// The lockout is enforced here rather than inside execTool on purpose: the
// engine's own execute_code calls (completion checks, chat_completion_check.go)
// go through execTool with the same name, and those are not the model's tool
// calls. Suppressing them would silently disable the checks that guard every
// run — the failure would look like "the check passed" (iron law 210).
func (h *ChatHarness) beforeToolCall(tc schemas.ToolCall, taskID string,
	emit func(ChatEvent) error, messages *[]ChatMessage) bool {
	h.checkDelegationDrift(tc, emit)
	if h.pd.blocksPoll(tc.Name, tc.Arguments) {
		// Name the task that was actually polled. With more than one hand-off
		// in the session, h.pd.taskID is only the most recent one, and a
		// refusal that quotes a different task than the call reads as a bug.
		polled, _ := tc.Arguments["task_id"].(string)
		if strings.TrimSpace(polled) == "" {
			polled = h.pd.taskID
		}
		msg := postDelegPollBlockedMessage(polled)
		log.Printf("[post-deleg] task=%s blocked poll n=%d", taskID, h.pd.pollBlocked)
		_ = emit(ChatEvent{Type: "post_deleg_poll_blocked", Data: tc.Name,
			Meta: map[string]any{"tool": tc.Name, "task": polled, "n": h.pd.pollBlocked}})
		_ = emit(ChatEvent{Type: "tool_result", Data: msg,
			Meta: map[string]any{"tool": tc.Name, "locked": true}})
		*messages = append(*messages, ChatMessage{
			Role:    "tool",
			Content: fmt.Sprintf("[tool %s] result:\n%s", tc.Name, msg),
		})
		return true
	}
	if !h.pd.blockedBy(tc.Name) {
		h.pd.noteExecuted()
		return false
	}
	msg := postDelegLockoutMessage(tc.Name, h.pd.taskID)
	log.Printf("[post-deleg] task=%s blocked tool=%s n=%d", taskID, tc.Name, h.pd.blocked)
	_ = emit(ChatEvent{Type: "post_deleg_lockout", Data: tc.Name,
		Meta: map[string]any{"tool": tc.Name, "task": h.pd.taskID, "n": h.pd.blocked}})
	_ = emit(ChatEvent{Type: "tool_result", Data: msg,
		Meta: map[string]any{"tool": tc.Name, "locked": true}})
	*messages = append(*messages, ChatMessage{
		Role:    "tool",
		Content: fmt.Sprintf("[tool %s] result:\n%s", tc.Name, msg),
	})
	return true
}

// blockedBy reports whether this tool call must be intercepted, and counts it.
func (p *postDelegState) blockedBy(name string) bool {
	if p.armMode() == PostDelegOff || !p.delegatedHere || !postDelegWorkTools[name] {
		return false
	}
	p.blocked++
	return true
}

// noteExecuted counts a tool call that actually ran after the delegation. It is
// a separate counter from blocked, on purpose: "how much work stayed in the
// foreground" and "how often the model tried to keep it there" are different
// quantities, and a single counter over both cannot attribute either (iron law
// 229). Read them together — off is (after=11, blocked=0), soft is likely
// (after small, blocked>0), hard is (after small, blocked≈0 because the tool
// is gone before the model can ask for it).
func (p *postDelegState) noteExecuted() {
	if p.delegatedHere {
		p.after++
	}
}

// SeedDelegatedTasks carries the tasks earlier turns handed off into this
// harness. A follow-up (wake) turn is a fresh ChatHarness with empty state, and
// a status check there used to be invisible to the poll lever — measured
// 2026-11-11, the single orchestrator_task_status call of a wake-arm run landed
// in the `_fu1` turn, so `post_deleg_polls` read 0 while the model did poll.
// The ids come from the handler's per-session ledger, not from the harness the
// delegation happened in: that instance is gone by the time the wake fires.
func (h *ChatHarness) SeedDelegatedTasks(ids []string) {
	if n := h.pd.seedDelegatedTasks(ids); n > 0 {
		log.Printf("[post-deleg] seeded %d delegated task(s) into the wake turn", n)
	}
}
