package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/chat"
)

// The fallback exists because 39 of 117 delegating runs in the archive (33%)
// ended with the wake turn hitting the request budget: artifacts complete, user
// gets nothing, median score 0.80, and no field in result.json says so. These
// tests pin the three properties the repair depends on: it fires when the turn
// cannot answer, it carries the real figures instead of an apology, and it is
// completely absent when the gate is off.

func fallbackOutcome(t *testing.T) taskOutcome {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "capacity_plan.json",
		`{"total_hourly_cost_usd":0.552,"selected_instance_type":"c7g.xlarge","required_capacity_rps":3108.56}`)
	return taskOutcome{
		Desc: "x", ID: "task-abc", Status: "completed",
		WorkspaceRoot: root, Declared: []string{"capacity_plan.json"},
	}
}

func TestWakeFallback_FiresWhenTheTurnCannotAnswer(t *testing.T) {
	t.Setenv("VORTEX_WAKE_FALLBACK", "on")
	h, store, provider := newFollowUpHandler(t, "")
	provider.err = errors.New("provider refused: max_agent_turns_exceeded")
	sess := store.GetOrCreate("s_fb")

	out := fallbackOutcome(t)
	report := wakeFallbackReport(out)
	if !h.startFollowUpTurnWithDigest("s_fb", "task-abc", out.summary(), "", report) {
		t.Fatal("follow-up turn was not started for an idle session")
	}
	h.wg.Wait()

	if provider.calls == 0 {
		t.Fatal("the turn never reached the provider, so the failure path was not taken")
	}
	// The user has to end up with the figures, not with an apology.
	if !waitForAssistantMessage(t, sess, "total_hourly_cost_usd: 0.552", 2*time.Second) {
		var got []string
		for _, m := range sess.SnapshotMessages() {
			got = append(got, m.Role+":"+m.Content)
		}
		t.Fatalf("the fallback never reached the session; messages=%v", got)
	}
	// And it must say why it is not prose, or the next reader will think the
	// model answered.
	if !waitForAssistantMessage(t, sess, "could not summarise", 2*time.Second) {
		t.Error("the report did not state why it is not a model answer")
	}
}

// A fallback that only lands in the session is not a delivery. The SSE reader
// stops at the first terminal event, so an `error` that reaches the stream first
// puts the figures behind a reload — measured on v93 (s93fb-on-r4): the report
// appeared once in chat/<session>.json and zero times in events.sse, and the
// stream ended on `error`. So the error has to be held, and the stream has to
// end on the report.
func TestWakeFallback_ReplacesTheErrorOnTheStream(t *testing.T) {
	t.Setenv("VORTEX_WAKE_FALLBACK", "on")
	h, store, provider := newFollowUpHandler(t, "")
	provider.err = errors.New("provider refused: max_agent_turns_exceeded")
	sess := store.GetOrCreate("s_fb_stream")

	out := fallbackOutcome(t)
	if !h.startFollowUpTurnWithDigest("s_fb_stream", "task-abc", out.summary(), "", wakeFallbackReport(out)) {
		t.Fatal("follow-up turn was not started for an idle session")
	}
	h.wg.Wait()

	var sawError, sawFallbackDone bool
	var lastTerminal, lastData string
	for _, ev := range sess.EventsAfter(0) {
		switch ev.Type {
		case "error":
			sawError = true
			lastTerminal, lastData = "error", ev.Data
		case "done":
			lastTerminal, lastData = "done", ev.Data
			if fb, _ := ev.Meta["fallback"].(bool); fb {
				sawFallbackDone = true
			}
		}
	}
	if sawError {
		t.Errorf("an error event reached the stream (%q); a reader stops there and never sees the report", lastData)
	}
	if !sawFallbackDone {
		t.Error("no done{fallback:true}: the report was never announced as this turn's answer")
	}
	if lastTerminal != "done" {
		t.Errorf("the stream ended on %q, want the fallback's done", lastTerminal)
	}
}

// The same failure with the gate off must still surface the error: holding a
// terminal event back is a property of the repair, not of the turn.
func TestWakeFallback_GateOffForwardsTheError(t *testing.T) {
	t.Setenv("VORTEX_WAKE_FALLBACK", "off")
	h, store, provider := newFollowUpHandler(t, "")
	provider.err = errors.New("provider refused: max_agent_turns_exceeded")
	sess := store.GetOrCreate("s_fb_stream_off")

	out := fallbackOutcome(t)
	if !h.startFollowUpTurnWithDigest("s_fb_stream_off", "task-abc", out.summary(), "", "") {
		t.Fatal("follow-up turn was not started for an idle session")
	}
	h.wg.Wait()

	var sawError, sawFallbackDone bool
	for _, ev := range sess.EventsAfter(0) {
		switch ev.Type {
		case "error":
			sawError = true
		case "done":
			if fb, _ := ev.Meta["fallback"].(bool); fb {
				sawFallbackDone = true
			}
		}
	}
	if !sawError {
		t.Error("gate off must not swallow the provider error")
	}
	if sawFallbackDone {
		t.Error("gate off produced a fallback done event")
	}
}

// The reason sentence and the log line are the same fact. They used to be built
// in two places, so one run could tell the user "the budget was spent" while the
// tooling recorded "no text was produced" (v93, s93fb-on-r4).
func TestWakeFallback_TextAndLogAgreeOnTheReason(t *testing.T) {
	t.Setenv("VORTEX_WAKE_FALLBACK", "on")
	h, store, _ := newFollowUpHandler(t, "")
	sess := store.GetOrCreate("s_fb_reason")
	terminalSent := true // delivery only: the report must not touch the stream here

	h.deliverWakeFallback(sess, wakeFallbackReport(fallbackOutcome(t)),
		wakeFallbackReasonNoText, &terminalSent)

	if !waitForAssistantMessage(t, sess, wakeFallbackReasonNoText, 2*time.Second) {
		t.Fatal("the delivered report does not carry the reason it was told to carry")
	}
	if !waitForAssistantMessage(t, sess, "total_hourly_cost_usd: 0.552", 2*time.Second) {
		t.Error("the delivered report lost the figures")
	}
}

func TestWakeFallback_GateOffKeepsTheOldSilence(t *testing.T) {
	t.Setenv("VORTEX_WAKE_FALLBACK", "off")
	h, store, provider := newFollowUpHandler(t, "")
	provider.err = errors.New("provider refused: max_agent_turns_exceeded")
	sess := store.GetOrCreate("s_fb_off")

	out := fallbackOutcome(t)
	if report := wakeFallbackReport(out); report != "" {
		t.Fatalf("gate off must produce no report; got:\n%s", report)
	}
	if !h.startFollowUpTurnWithDigest("s_fb_off", "task-abc", out.summary(), "", "") {
		t.Fatal("follow-up turn was not started")
	}
	h.wg.Wait()

	for _, m := range sess.SnapshotMessages() {
		if m.Role == "assistant" && strings.Contains(m.Content, "total_hourly_cost_usd") {
			t.Fatalf("gate off must preserve the pre-fix silence; got message:\n%s", m.Content)
		}
	}
}

func TestWakeFallback_EmptyAnswerIsAlsoAFallback(t *testing.T) {
	// A turn that ran and said nothing is the same loss as one that never ran:
	// both leave the artifacts unread. dig-r3 in the 120-turn batch was exactly
	// this shape (82 requests, no assistant text, score 1.00).
	t.Setenv("VORTEX_WAKE_FALLBACK", "on")
	h, store, _ := newFollowUpHandler(t, "")
	sess := store.GetOrCreate("s_fb_empty")

	out := fallbackOutcome(t)
	report := wakeFallbackReport(out)
	if !h.startFollowUpTurnWithDigest("s_fb_empty", "task-abc", out.summary(), "", report) {
		t.Fatal("follow-up turn was not started")
	}
	h.wg.Wait()

	if !waitForAssistantMessage(t, sess, "total_hourly_cost_usd: 0.552", 2*time.Second) {
		t.Error("an empty answer must fall back to the artifact report")
	}
}

func TestWakeFallback_TerminalEventCarriesTheFlag(t *testing.T) {
	// The SSE reader stops at the first terminal event. If the fallback were
	// appended after `error`, it would sit in the database and never reach the
	// user who is waiting — so when nothing terminal has been sent, this path
	// sends its own `done` and marks it.
	t.Setenv("VORTEX_WAKE_FALLBACK", "on")
	h, store, _ := newFollowUpHandler(t, "")
	sess := store.GetOrCreate("s_fb_ev")

	terminal := false
	report := wakeFallbackReport(fallbackOutcome(t))
	h.deliverWakeFallback(sess, report, wakeFallbackReasonBudget, &terminal)

	if !terminal {
		t.Error("deliverWakeFallback must send a terminal event when none was sent")
	}
	found := false
	for _, ev := range sess.EventsAfter(0) {
		if ev.Type == "done" {
			if v, _ := ev.Meta["fallback"].(bool); v {
				found = true
			}
		}
	}
	if !found {
		t.Error("the done event must be marked fallback=true, or a local report is indistinguishable from a model answer")
	}
	if !strings.Contains(report, "0.552") {
		t.Errorf("report lost the headline figure:\n%s", report)
	}
}

func TestWakeFallback_ReportShape(t *testing.T) {
	t.Setenv("VORTEX_WAKE_FALLBACK", "on")

	// No declared artifacts: say that, do not invent a result.
	empty := wakeFallbackReport(taskOutcome{ID: "t1", WorkspaceRoot: t.TempDir()})
	if !strings.Contains(empty, "declared no output files") {
		t.Errorf("a task with no declared artifacts must be reported as such:\n%s", empty)
	}

	// Declared but nothing readable: name them and say so. A message that looks
	// like a result and contains none is worse than silence.
	missing := wakeFallbackReport(taskOutcome{
		ID: "t2", WorkspaceRoot: t.TempDir(), Declared: []string{"gone.json"},
	})
	if !strings.Contains(missing, "gone.json") || !strings.Contains(missing, "could be read back") {
		t.Errorf("unreadable declared artifacts must be named:\n%s", missing)
	}

	// Readable: the numbers, and the sentence that says where they came from.
	full := wakeFallbackReport(fallbackOutcome(t))
	if !strings.Contains(full, "read straight out of the files") {
		t.Errorf("a report with content must state its provenance:\n%s", full)
	}
}

// TestWakeFallback_IsDeliveryOnly is the control that keeps this repair honest:
// with the gate on and a *successful* turn, the fallback must not appear at all.
// Otherwise "the user got figures" could not be attributed to the model.
func TestWakeFallback_IsDeliveryOnly(t *testing.T) {
	t.Setenv("VORTEX_WAKE_FALLBACK", "on")
	h, store, _ := newFollowUpHandler(t, "the plan costs $0.552 per hour")
	sess := store.GetOrCreate("s_fb_ok")

	out := fallbackOutcome(t)
	report := wakeFallbackReport(out)
	if !h.startFollowUpTurnWithDigest("s_fb_ok", "task-abc", out.summary(), "", report) {
		t.Fatal("follow-up turn was not started")
	}
	h.wg.Wait()

	if !waitForAssistantMessage(t, sess, "the plan costs", 2*time.Second) {
		t.Fatal("the model's own answer did not land")
	}
	for _, m := range sess.SnapshotMessages() {
		if strings.Contains(m.Content, "could not summarise") {
			t.Errorf("the fallback fired on a successful turn:\n%s", m.Content)
		}
	}
	_ = chat.ChatMessage{} // keep the import meaningful if the assertions change
}
