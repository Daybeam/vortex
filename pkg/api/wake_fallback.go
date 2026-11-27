package api

import (
	"fmt"
	"os"
	"strings"

	"github.com/daybeam/vortex/pkg/chat"
)

// Wake-turn fallback — what the user gets when the summary turn cannot answer.
//
// Measured on 2026-11-18 across the harness-bench archive (→_wake_completion.py,
// 117 classifiable runs that delegated): 69 (59%) ended with a finished wake turn,
// 40 (34%) were refused, 4 were cancelled by the round deadline, 4 errored after
// real work. Those 48 delivered nothing while averaging 0.680 with 15 perfect
// scores: the artifacts were fine, the user got nothing, and no field in
// result.json says so (D22).
//
// The refusal shape is specific, and it is not "ran out of turns mid-answer":
// 37 of the 40 exit the follow-up turn in `elapsed=0.0s` with zero tool calls,
// i.e. the turn was admitted and instantly discarded. The split is deterministic
// — requests == max_agent_turns → 37/41 refused and 0 completed; requests <
// max_agent_turns → 0/76 refused and 69 completed. So the shared request pool,
// not the model, is what refuses here (D23); the separate wall-clock case is D25.
//
// The code path was one early return:
//
//	if err != nil && !chat.IsPartialRun(err) {
//	    fmt.Fprintf(os.Stderr, "… turn failed: %v\n", err)
//	    return                      // ← nothing appended, nothing emitted
//	}
//
// A model turn is the only way to *summarise* a result, and the budget that pays
// for it is shared with the sub-agent that produced the result. When the
// sub-agent wins that race the summary loses, which is why the answer is always
// last in line (D23 for the request budget, D25 for the wall clock).
//
// But a summary does not need a model to be *useful*. The engine already knows
// which files the task declared and can render their headline figures locally —
// that is what wake_seed_digest.go does for the prompt, and the same bounded
// renderer serves here for the session. So the fallback is not an apology, it is
// the numbers, unparaphrased, plus one sentence saying why it is not prose.
//
// Gated separately from VORTEX_WAKE_SEED_DIGEST on purpose: the digest changes
// what the model sees (a treatment), the fallback changes what the user gets
// when the model never runs (a repair). Measuring them under one switch would
// make each one's effect unattributable.
const (
	// wakeFallbackReasonBudget is the common case: the provider refused the
	// request because the run's request budget was already spent by the
	// sub-agent. Phrasing it as a budget fact, not a failure, is deliberate —
	// "the summary could not run" is true, "the task failed" is not.
	wakeFallbackReasonBudget = "the model could not be reached — this run's request budget was already spent by the delegated task"
	// wakeFallbackReasonDeadline is the wall-clock twin (D25).
	wakeFallbackReasonDeadline = "the round deadline arrived before the summary could be written"
	// wakeFallbackReasonNoText covers a turn that ran and produced nothing:
	// an empty completion, a tool-only reply, a partial run.
	wakeFallbackReasonNoText = "the summary turn ended without producing any text"
)

// wakeFallbackMode reads VORTEX_WAKE_FALLBACK. Default off, like every other
// behavioural change in this package.
func wakeFallbackMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VORTEX_WAKE_FALLBACK"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// wakeFallbackOpening is the one sentence that explains why this is not prose.
//
// It is written at delivery time, never at build time, because delivery is the
// only place that knows the actual reason. Building it earlier gave one concept
// two paths: the report was assembled with the budget wording while the log line
// recorded "ended without producing any text", so the same run told the user one
// cause and the tooling another (measured on v93, s93fb-on-r4).
func wakeFallbackOpening(reason string) string {
	return "The delegated task finished, but I could not summarise it: " + reason + ".\n\n"
}

// wakeFallbackReport builds the body of the message: the numbers, read out of the
// files, with the reason sentence added by deliverWakeFallback. Empty when the
// gate is off, so the caller can pass the result straight through and the
// pre-fix behaviour is preserved exactly.
//
// It does NOT consult wakeSeedDigestMode: the digest gate decides what a *model*
// is allowed to see, and there is no model here. Sharing the gate would mean the
// repair silently disappears whenever the treatment is off — which is precisely
// the arm that needs it most (its wakes fail more often, because the sub-agent
// gets the whole budget).
func wakeFallbackReport(o taskOutcome) string {
	if !wakeFallbackMode() {
		return ""
	}
	var b strings.Builder

	if o.WorkspaceRoot == "" || len(o.Declared) == 0 {
		b.WriteString("It declared no output files, so there is nothing to report here. ")
		b.WriteString("Check the task record for what it actually did.\n")
		return b.String()
	}
	digest, _ := buildArtifactDigest(o.WorkspaceRoot, o.Declared, wakeDigestBudgetChars)
	if digest == "" {
		// Declared but unreadable. Saying so is the point: the alternative is a
		// message that looks like a result and contains none.
		b.WriteString("It declared ")
		b.WriteString(strings.Join(o.Declared, ", "))
		b.WriteString(", but none of them could be read back. Check the workspace before relying on them.\n")
		return b.String()
	}
	b.WriteString("Nothing below is paraphrased — it is read straight out of the files it wrote:\n")
	b.WriteString(digest)
	return b.String()
}

// deliverWakeFallback puts the report in front of the user: the session tree (so a
// reload sees it), the event stream (so a live client sees it), and one log line
// (so the batch tooling can tell whether it fired at all).
//
// The terminal event matters. Run emits `error` when the provider refused the
// request, and the SSE reader stops at the first terminal event — a message
// appended afterwards would exist in the database and never reach the user who
// was waiting. So when nothing terminal has been sent yet, this sends `delta`
// followed by a `done` carrying fallback=true: the turn is over, and what the
// user got is this report. When the terminal was already sent (a hard error),
// the message still goes into the session tree, and the log line records that it
// arrived late.
func (h *ChatHandler) deliverWakeFallback(session *chat.ChatSession, report, reason string, terminalSent *bool) {
	if report == "" || session == nil {
		return
	}
	// The reason sentence is added here, not baked into the body upstream, so the
	// delivered text and the log line cannot disagree about why this happened.
	report = wakeFallbackOpening(reason) + report
	wakeFallbackLog(session.ID, reason, len(report))
	if terminalSent != nil && !*terminalSent {
		session.AddEvent(chat.ChatEvent{Type: "delta", Data: report})
		session.AddEvent(chat.ChatEvent{
			Type: "done",
			Data: "chat turn ended; the summary is a local report, not a model answer",
			Meta: map[string]any{"fallback": true, "reason": reason},
		})
		*terminalSent = true
	}
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: report}, "")
	h.persistSession(session)
}

// wakeFallbackLog is one line the batch tooling can count. The content itself
// lives in the session tree; a run's answer is otherwise invisible to anything
// that only reads result.json (D22), and "did a fallback fire" is the one bit
// that decides whether a run delivered anything at all.
func wakeFallbackLog(taskID, reason string, chars int) {
	fmt.Fprintf(os.Stderr, "[chat-followup] task=%s fallback=on reasons=%q chars=%d\n",
		taskID, reason, chars)
}
