package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/chat"
)

// These pin the push path built in chat_wake.go: a task state that cannot
// resolve itself must reach the *model*, not just the SSE queue. Before this,
// decision_required did session.AddEvent and stopped there, so a blocked task
// had a visible event and no agent that knew about it.

func TestDecisionWakeText_CoversBothPayloadShapes(t *testing.T) {
	// Shape 1: the decision gate sends {decision_id, type, options}.
	gate := decisionWakeText("task_1", "s2", map[string]any{
		"decision_id": "dec_1",
		"type":        "approval",
		"options":     []string{"approve", "reject"},
	})
	for _, want := range []string{"task_1", "s2", "approval", "approve, reject", "Ask the user"} {
		if !strings.Contains(gate, want) {
			t.Errorf("gate payload text is missing %q:\n%s", want, gate)
		}
	}

	// Shape 2: the max-turns path sends {reason, error, retry_count, resume_hint}
	// and no options at all. A renderer that only knows shape 1 produces an
	// empty instruction here, which is how most steps actually block.
	maxTurns := decisionWakeText("task_2", "s1", map[string]any{
		"reason":      "max_tool_turns_exhausted",
		"error":       "step exhausted its budget",
		"retry_count": 2,
		"resume_hint": "choose resume_more_turns to continue",
	})
	for _, want := range []string{"max_tool_turns_exhausted", "step exhausted its budget", "resume_more_turns"} {
		if !strings.Contains(maxTurns, want) {
			t.Errorf("max-turns payload text is missing %q:\n%s", want, maxTurns)
		}
	}
	if strings.Contains(maxTurns, "Options:") {
		t.Errorf("must not render an empty Options line:\n%s", maxTurns)
	}
}

func TestWakeMain_StartsTurnWhenIdle(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "which option do you want?")
	sess := store.GetOrCreate("s_wake")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "start the task"}, "")

	if !h.wakeMain("s_wake", "task_w1", "decision", "task needs a decision", "dec_1") {
		t.Fatal("wakeMain returned false for an idle session")
	}
	h.wg.Wait()

	if provider.calls == 0 {
		t.Fatal("provider was never called; the wake did not start a turn")
	}
	if !waitForAssistantMessage(t, sess, "which option", 2*time.Second) {
		t.Fatalf("no answer in the session; messages=%+v", sess.SnapshotMessages())
	}
	sawFollowUp := false
	for _, ev := range sess.EventsAfter(0) {
		if ev.Type == "followup_turn" {
			sawFollowUp = true
		}
	}
	if !sawFollowUp {
		t.Error("the wake must announce itself with a followup_turn event")
	}
	if _, held := h.cancelFuncs.Load("s_wake"); held {
		t.Error("wake did not release the cancelFuncs slot")
	}
}

func TestWakeMain_DedupesByKey(t *testing.T) {
	h, store, _ := newFollowUpHandler(t, "ok")
	store.GetOrCreate("s_dup")

	if !h.wakeMain("s_dup", "task_d", "decision", "needs a decision", "dec_9") {
		t.Fatal("first wake must start a turn")
	}
	if h.wakeMain("s_dup", "task_d", "decision", "needs a decision", "dec_9") {
		t.Error("the same decision must not be delivered twice")
	}
	h.wg.Wait()
}

func TestWakeMain_DoesNotTouchALiveTurn(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "ok")
	store.GetOrCreate("s_busy")

	// Stand in for the user's in-flight turn: the slot is taken and holds that
	// turn's cancel func. A wake must neither replace it nor cancel it.
	cancelled := false
	h.cancelFuncs.Store("s_busy", context.CancelFunc(func() { cancelled = true }))

	if !h.wakeMain("s_busy", "task_b", "decision", "needs a decision", "dec_b") {
		t.Error("wakeMain must queue the wake even when a turn is in flight")
	}
	if cancelled {
		t.Error("wakeMain cancelled the live turn's context")
	}
	if provider.calls != 0 {
		t.Errorf("provider was called %d times; no turn should have started", provider.calls)
	}
	if v, ok := h.cancelFuncs.Load("s_busy"); !ok || v == nil {
		t.Error("wakeMain removed the live turn's cancel func")
	}

	// The wake must be queued for finishTurn to deliver after the turn ends.
	if v, ok := h.pendingWake.Load("s_busy"); !ok || v == nil {
		t.Fatal("wake was not queued in pendingWake")
	} else {
		q := v.(*wakeQueue)
		queued := q.snapshot()
		if len(queued) != 1 {
			t.Fatalf("expected exactly 1 queued wake, got %d: %+v", len(queued), queued)
		}
		if queued[0].taskID != "task_b" || queued[0].text != "needs a decision" {
			t.Errorf("queued wake has wrong data: %+v", queued[0])
		}
		if queued[0].reason != "wake:decision" {
			t.Errorf("queued reason must be normalised to the starter's form, got %q", queued[0].reason)
		}
		// Pushed state is budgeted; an unbudgeted flag here would let a decision
		// notice bypass wakeMaxPerTask and turn the session into a log stream.
		if !queued[0].budgeted {
			t.Error("a decision wake must be queued as budgeted")
		}
	}

	// The reservation must be rolled back: queuing is not the task's fault,
	// and the next attempt (after the turn ends) has to be possible.
	h.pendingWake.Delete("s_busy") // simulate finishTurn consuming the queue
	h.cancelFuncs.Delete("s_busy") // simulate the turn ending
	if !h.wakeMain("s_busy", "task_b", "decision", "needs a decision", "dec_b2") {
		t.Error("a queued wake must not consume the task's wake budget")
	}
	h.wg.Wait()
}

// TestFinishTurn_DeliversQueuedWake verifies the P4 fallback (Option D):
// when a wake was deferred because the session was busy, finishTurn delivers
// it after the current turn ends — the model sees the decision without
// waiting for the next user message.
func TestFinishTurn_DeliversQueuedWake(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "the decision is: approve")
	sess := store.GetOrCreate("s_fu")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "run the task"}, "")

	// Simulate a wake that was deferred while the session was busy.
	q := &wakeQueue{}
	q.push(pendingWakeEntry{
		taskID: "task_dec",
		reason: "wake:decision",
		text:   "task_dec is blocked, choose: approve or reject",
	})
	h.pendingWake.Store("s_fu", q)

	// finishTurn runs after the turn's cancelFuncs slot is freed.
	h.finishTurn(sess, true, nil)

	// Wait for the follow-up goroutine to complete.
	h.wg.Wait()

	// A follow-up turn must have been started to deliver the queued wake.
	if provider.calls == 0 {
		t.Fatal("provider was never called; finishTurn did not deliver the queued wake")
	}

	if !waitForAssistantMessage(t, sess, "approve", 2*time.Second) {
		t.Fatalf("no answer from the delivered wake; messages=%+v", sess.SnapshotMessages())
	}

	// The queued wake must have been consumed, not delivered twice. The queue
	// object itself is kept for the session's lifetime (deleting it races an
	// enqueue), so the property is "empty and does not fire again".
	v, ok := h.pendingWake.Load("s_fu")
	if !ok {
		t.Fatal("the queue object disappeared; an enqueue racing the drain can be lost")
	}
	if got := v.(*wakeQueue).snapshot(); len(got) != 0 {
		t.Fatalf("pendingWake was not consumed by finishTurn: %+v", got)
	}
	calls := provider.calls
	h.finishTurn(sess, true, nil)
	h.wg.Wait()
	if provider.calls != calls {
		t.Error("a second finishTurn re-delivered the consumed wake")
	}
}

func TestWakeMain_ThrottlesBurst(t *testing.T) {
	h, store, _ := newFollowUpHandler(t, "ok")
	store.GetOrCreate("s_burst")

	if !h.wakeMain("s_burst", "task_x", "decision", "first", "d1") {
		t.Fatal("first wake must start a turn")
	}
	if h.wakeMain("s_burst", "task_x", "decision", "second", "d2") {
		t.Error("a second wake within the interval must be throttled")
	}
	h.wg.Wait()
}

// TestDeliverPendingWake_TaskResultIgnoresDecisionBudget pins a distinction the
// first version of this queue got wrong: the per-task wake budget exists to stop
// a blocked task from streaming notices at the model, so it applies to pushed
// state only. Applying it to the drain path as well would drop a finished task's
// result whenever that same task had previously used up its decision wakes —
// silent data loss, reintroduced by a well-meaning uniformity.
func TestDeliverPendingWake_TaskResultIgnoresDecisionBudget(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "delivered anyway")
	sess := store.GetOrCreate("s_nobudget")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "run the task"}, "")

	h.cancelFuncs.Store("s_nobudget", context.CancelFunc(func() {}))
	if h.startFollowUpTurn("s_nobudget", "task-nb", "notice") {
		t.Fatal("follow-up started while the session was busy")
	}

	// The same task already burned its whole decision-wake budget.
	st := h.wakeStateFor("task-nb")
	st.mu.Lock()
	st.total = wakeMaxPerTask
	st.mu.Unlock()

	h.cancelFuncs.Delete("s_nobudget")
	h.finishTurn(sess, true, nil)
	h.wg.Wait()

	if provider.calls == 0 {
		t.Fatal("a finished task's result must not be dropped by the decision-wake budget")
	}
	if !waitForAssistantMessage(t, sess, "delivered anyway", 2*time.Second) {
		t.Fatalf("no answer; messages=%+v", sess.SnapshotMessages())
	}
}

// TestEnqueueWake_KeepsEveryEntry pins the FIFO. The queue used to be a single
// slot where a newer wake replaced the older one, which was defensible for
// decisions (the newest is the most actionable) and catastrophic for finished
// tasks: a session with two delegated tasks lost the first result with no log
// line, because the second completion overwrote it before finishTurn drained.
func TestEnqueueWake_KeepsEveryEntry(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "answered")
	sess := store.GetOrCreate("s_fifo")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "run both"}, "")

	// A user turn holds the session while both tasks finish.
	h.cancelFuncs.Store("s_fifo", context.CancelFunc(func() {}))
	if h.startFollowUpTurn("s_fifo", "task-a", "notice") {
		t.Fatal("follow-up started while the session was busy")
	}
	if h.startFollowUpTurn("s_fifo", "task-b", "notice") {
		t.Fatal("follow-up started while the session was busy")
	}
	v, ok := h.pendingWake.Load("s_fifo")
	if !ok {
		t.Fatal("nothing was queued")
	}
	if got := len(v.(*wakeQueue).snapshot()); got != 2 {
		t.Fatalf("expected both wakes queued, got %d (the single-slot bug)", got)
	}

	// The turn ends; the chain delivers one wake per turn.
	h.cancelFuncs.Delete("s_fifo")
	h.finishTurn(sess, true, nil)
	h.wg.Wait()

	if provider.calls != 2 {
		t.Errorf("expected one provider call per delivered wake (2), got %d", provider.calls)
	}
	if n := countEvents(sess, "followup_turn"); n != 2 {
		t.Errorf("expected exactly 2 followup_turn events, got %d", n)
	}
	if v, ok := h.pendingWake.Load("s_fifo"); ok && !v.(*wakeQueue).empty() {
		t.Errorf("queue not drained: %+v", v.(*wakeQueue).snapshot())
	}
}

// TestDeliverPendingWake_SkipsConsumedResult pins the `off` arm — the original
// rule, kept so archived runs stay comparable. The default arm is `read`
// (VORTEX_WAKE_CONTENT_SUPPRESS unset), where the same marker is no longer
// enough on its own: see TestDeliverPendingWake_ReadMode*.
func TestDeliverPendingWake_SkipsConsumedResult(t *testing.T) {
	t.Setenv(wakeContentSuppressEnv, "off")
	h, store, provider := newFollowUpHandler(t, "should not appear")
	sess := store.GetOrCreate("s_consumed")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "run the task"}, "")

	if !h.enqueueWake("s_consumed", pendingWakeEntry{
		taskID: "task-c", reason: "task_done", text: "task-c finished",
	}) {
		t.Fatal("wake was not queued")
	}
	h.noteTaskConsumed("s_consumed", "task-c")

	h.finishTurn(sess, true, nil)
	h.wg.Wait()

	if provider.calls != 0 {
		t.Errorf("a consumed result must not start a turn, got %d provider call(s)", provider.calls)
	}
	if n := countEvents(sess, "followup_turn"); n != 0 {
		t.Errorf("a consumed result must announce nothing, got %d event(s)", n)
	}
	if v, ok := h.pendingWake.Load("s_consumed"); ok && !v.(*wakeQueue).empty() {
		t.Errorf("queue not drained after suppression: %+v", v.(*wakeQueue).snapshot())
	}
	// The suppressed cause must count as delivered: a repeat terminal event for
	// the same task has to be refused by the dedupe set, not queued again.
	if _, dup := h.followUpSeen.Load("s_consumed|task-c|task_done"); !dup {
		t.Error("a suppressed wake must still be marked delivered, or the cause returns")
	}
}

// TestDeliverPendingWake_KeepsEntryWhenStillBusy pins the drop-on-reclaim rule:
// if another turn grabbed the session between finishTurn freeing the slot and
// the delivery attempt, the entry goes back to the head of the queue instead of
// being lost.
func TestDeliverPendingWake_KeepsEntryWhenStillBusy(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "delivered later")
	sess := store.GetOrCreate("s_still")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "run the task"}, "")

	if !h.enqueueWake("s_still", pendingWakeEntry{
		taskID: "task-s", reason: "task_done", text: "task-s finished",
	}) {
		t.Fatal("wake was not queued")
	}

	// A new turn claimed the session before the drain ran.
	h.cancelFuncs.Store("s_still", context.CancelFunc(func() {}))
	h.finishTurn(sess, true, nil)

	v, ok := h.pendingWake.Load("s_still")
	if !ok {
		t.Fatal("the wake was lost when the reclaim failed")
	}
	queued := v.(*wakeQueue).snapshot()
	if len(queued) != 1 || queued[0].taskID != "task-s" {
		t.Fatalf("entry did not survive the failed reclaim: %+v", queued)
	}
	if provider.calls != 0 {
		t.Errorf("no turn may start while the session is busy, got %d call(s)", provider.calls)
	}

	// The next turn ends and the entry is delivered.
	h.cancelFuncs.Delete("s_still")
	h.finishTurn(sess, true, nil)
	h.wg.Wait()
	if provider.calls == 0 {
		t.Fatal("the retained wake never ran")
	}
	if !waitForAssistantMessage(t, sess, "delivered later", 2*time.Second) {
		t.Fatalf("no answer; messages=%+v", sess.SnapshotMessages())
	}
}

// TestDeliverPendingWake_ReadModeDropsOnlyWhenArtifactRead is the default arm:
// the terminal-state marker is a precondition, and the evidence that actually
// justifies a drop is that this turn read one of the task's declared artifacts.
//
// The marker alone was the whole rule until 2026-11-23. Over 283 archived
// firings the model had read no declared artifact in 137 of them (48%) — the
// wake was the only channel, and the rule closed it.
func TestDeliverPendingWake_ReadModeDropsOnlyWhenArtifactRead(t *testing.T) {
	t.Setenv(wakeContentSuppressEnv, "read")
	h, store, provider := newFollowUpHandler(t, "should not appear")
	sess := store.GetOrCreate("s_read_drop")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "run the task"}, "")

	if !h.enqueueWake("s_read_drop", pendingWakeEntry{
		taskID: "task-r", reason: "task_done", text: "task-r finished",
		declared: []string{"plan.json", "notes.md"},
	}) {
		t.Fatal("wake was not queued")
	}
	h.noteTaskConsumed("s_read_drop", "task-r")
	// Base name only: read_file is called with a container path, the declared
	// list carries a relative one, and the wake entry is what joins them.
	h.noteFileRead("s_read_drop", "plan.json")

	h.finishTurn(sess, true, nil)
	h.wg.Wait()

	if provider.calls != 0 {
		t.Errorf("a wake whose artifact this turn read must not start a turn, got %d call(s)", provider.calls)
	}
	if n := countEvents(sess, "followup_turn"); n != 0 {
		t.Errorf("a suppressed wake must announce nothing, got %d event(s)", n)
	}
	if _, dup := h.followUpSeen.Load("s_read_drop|task-r|task_done"); !dup {
		t.Error("a suppressed wake must still be marked delivered, or the cause returns")
	}
}

// TestDeliverPendingWake_ReadModeDeliversWithoutReadEvidence is the same setup
// with the read removed: the marker is set, but the turn never read the
// artifact, so the wake must be delivered.
func TestDeliverPendingWake_ReadModeDeliversWithoutReadEvidence(t *testing.T) {
	t.Setenv(wakeContentSuppressEnv, "read")
	h, store, provider := newFollowUpHandler(t, "delivered result")
	sess := store.GetOrCreate("s_read_keep")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "run the task"}, "")

	if !h.enqueueWake("s_read_keep", pendingWakeEntry{
		taskID: "task-k", reason: "task_done", text: "task-k finished",
		declared: []string{"plan.json"},
	}) {
		t.Fatal("wake was not queued")
	}
	h.noteTaskConsumed("s_read_keep", "task-k")
	// A read of some *other* file must not count as evidence for this task.
	h.noteFileRead("s_read_keep", "unrelated.md")

	h.finishTurn(sess, true, nil)
	h.wg.Wait()

	if provider.calls == 0 {
		t.Fatal("the wake was dropped without read evidence — the only channel closed")
	}
	if !waitForAssistantMessage(t, sess, "delivered result", 2*time.Second) {
		t.Fatalf("no answer; messages=%+v", sess.SnapshotMessages())
	}
}

// TestDeliverPendingWake_ReadEvidenceIsClearedPerTurn pins the lifetime: read
// evidence describes the turn that did the reading, so the next turn must not
// inherit it and drop a wake for a task it never touched.
func TestDeliverPendingWake_ReadEvidenceIsClearedPerTurn(t *testing.T) {
	t.Setenv(wakeContentSuppressEnv, "read")
	h, _, _ := newFollowUpHandler(t, "")
	h.noteFileRead("s_scope", "plan.json")
	if got := h.readDeclared("s_scope", []string{"plan.json"}); len(got) != 1 {
		t.Fatalf("read evidence not recorded: %+v", got)
	}
	h.clearReadFiles("s_scope")
	if got := h.readDeclared("s_scope", []string{"plan.json"}); len(got) != 0 {
		t.Errorf("read evidence survived the turn boundary: %+v", got)
	}
	// Another session's identical read must not be visible here either.
	h.noteFileRead("s_other", "plan.json")
	if got := h.readDeclared("s_scope", []string{"plan.json"}); len(got) != 0 {
		t.Errorf("read evidence leaked across sessions: %+v", got)
	}
}
