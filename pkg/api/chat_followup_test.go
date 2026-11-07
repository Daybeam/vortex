package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
)

// Tests for the follow-up turn — segment ④ of async delegation (chat_followup.go).
//
// The invariant these pin is *ownership*: exactly one thing may drive a chat
// session at a time. Get that wrong in either direction and the damage is
// silent:
//   - a follow-up that starts while a turn is in flight produces two answers,
//     and can kill the user's turn (SendHandler cancels whatever sits in
//     cancelFuncs);
//   - a follow-up that never starts leaves the delegated result in the session
//     tree unread, which is the bug this whole file exists to fix.

// followUpProvider answers every round with plain text, i.e. one provider call
// per turn and no tool calls.
type followUpProvider struct {
	answer string
	calls  int
	lastIn string
}

func (p *followUpProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *followUpProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	p.lastIn = req.System + "\n" + req.User
	onChunk(p.answer)
	return &schemas.ProviderResponse{Text: p.answer}, nil
}

func (p *followUpProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (p *followUpProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (p *followUpProvider) Name() string                                              { return "mock-followup" }

func newFollowUpHandler(t *testing.T, answer string) (*ChatHandler, *chat.ChatSessionStore, *followUpProvider) {
	t.Helper()
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	provider := &followUpProvider{answer: answer}
	return &ChatHandler{
		Store: store,
		Harness: &chat.ChatHarness{
			Provider:   provider,
			Model:      "mock",
			OutputBase: t.TempDir(),
			MaxTurns:   4,
		},
	}, store, provider
}

// waitForAssistantMessage polls until the session holds an assistant message
// containing want, or the deadline passes. The follow-up runs in a goroutine,
// so the test cannot just read the store straight after the call.
func waitForAssistantMessage(t *testing.T, sess *chat.ChatSession, want string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, m := range sess.SnapshotMessages() {
			if m.Role == "assistant" && strings.Contains(m.Content, want) {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// countEvents counts events of one type in the session ring.
func countEvents(sess *chat.ChatSession, typ string) int {
	n := 0
	for _, ev := range sess.EventsAfter(0) {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// TestFollowUpTurn_RunsWhenSessionIsIdle is the happy path: the task finished
// after its turn ended, so the session gets one more turn and the answer lands
// in the tree.
func TestFollowUpTurn_RunsWhenSessionIsIdle(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "the review found two blocking issues")
	sess := store.GetOrCreate("s_idle")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "review the diff"}, "")
	before := len(sess.SnapshotMessages())

	if !h.startFollowUpTurn("s_idle", "task-abc", "[Task Notification] done") {
		t.Fatal("follow-up turn was not started for an idle session")
	}
	if !waitForAssistantMessage(t, sess, "two blocking issues", 5*time.Second) {
		t.Fatalf("no follow-up answer in the session; messages=%+v", sess.SnapshotMessages())
	}
	// The answer is appended *inside* the goroutine, before its defers run, so
	// seeing the message does not mean the turn is over. Without this wait the
	// slot assertion below races the goroutine and fails ~40% of runs (observed).
	h.wg.Wait()
	if provider.calls == 0 {
		t.Fatal("provider was never called")
	}

	msgs := sess.SnapshotMessages()
	if len(msgs) != before+1 {
		t.Fatalf("follow-up must add exactly one message (the answer), got %d → %d: %+v",
			before, len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content, "System notice") {
			t.Errorf("the transient trigger must never be persisted as a user message, got: %s", m.Content)
		}
	}
	if last := msgs[len(msgs)-1]; last.Role != "assistant" {
		t.Errorf("last message must be the assistant answer, got role=%s", last.Role)
	}

	// A turn without a terminal event leaves the SSE stream open; the follow-up
	// must emit exactly one.
	terminal := 0
	for _, ev := range sess.EventsAfter(0) {
		if ev.Type == "done" || ev.Type == "error" {
			terminal++
		}
	}
	if terminal != 1 {
		t.Errorf("follow-up must leave exactly 1 terminal event, got %d", terminal)
	}

	// The session slot must be released, or the next user message would be
	// treated as "a turn is in flight" and the follow-up bookkeeping would leak.
	if _, held := h.cancelFuncs.Load("s_idle"); held {
		t.Error("follow-up did not release the cancelFuncs slot")
	}

	// The announcement is a contract, not telemetry: a client that reconnected
	// after `done` uses its presence to decide "keep reading" and its absence to
	// decide "no turn is coming". Exactly one per started turn.
	if n := countEvents(sess, "followup_turn"); n != 1 {
		t.Errorf("expected exactly 1 followup_turn event, got %d", n)
	}
}

// TestFollowUpTurn_QueuesWhenTurnInFlight pins the load-bearing safety rule and
// its 2026-11-01 amendment. A turn owns the session, so no follow-up may start
// now and the live turn's cancel func must not be replaced. But the wake must be
// QUEUED, not dropped: dropping it is what lost the delegated result on
// harness-bench p24 — one log line, no answer, and the turn had every reason to
// report success.
func TestFollowUpTurn_QueuesWhenTurnInFlight(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "the deferred answer")
	sess := store.GetOrCreate("s_busy")
	sess.AppendMessage(chat.ChatMessage{Role: "user", Content: "run the task"}, "")

	cancelled := false
	liveCancel := context.CancelFunc(func() { cancelled = true })
	h.cancelFuncs.Store("s_busy", liveCancel)

	if h.startFollowUpTurn("s_busy", "task-busy", "notice") {
		t.Fatal("follow-up started while a turn was in flight")
	}
	if provider.calls != 0 {
		t.Errorf("provider called %d time(s) for a deferred follow-up", provider.calls)
	}
	got, ok := h.cancelFuncs.Load("s_busy")
	if !ok {
		t.Fatal("the in-flight turn's cancel func was removed")
	}
	if cancelled {
		t.Error("the in-flight turn was cancelled by a deferred follow-up")
	}
	// Calling the stored func must still be the *live turn's* func, not ours.
	got.(context.CancelFunc)()
	if !cancelled {
		t.Error("the stored cancel func is no longer the in-flight turn's")
	}

	// Nothing is announced yet: the announcement marks a turn that started, and
	// a client must not be told to wait for a turn that has not begun.
	if n := countEvents(sess, "followup_turn"); n != 0 {
		t.Errorf("a deferred follow-up must announce nothing yet, got %d event(s)", n)
	}

	// The wake is queued with the reason the starter itself would have used, so
	// the dedupe key still matches when it is finally delivered.
	v, ok := h.pendingWake.Load("s_busy")
	if !ok {
		t.Fatal("the wake was dropped instead of queued")
	}
	queued := v.(*wakeQueue).snapshot()
	if len(queued) != 1 || queued[0].taskID != "task-busy" || queued[0].reason != "task_done" {
		t.Fatalf("queued wake is wrong: %+v", queued)
	}

	// The turn ends — its own defer frees the slot before finishTurn runs — and
	// the queued wake is delivered exactly once.
	h.cancelFuncs.Delete("s_busy")
	h.finishTurn(sess, true, nil)
	h.wg.Wait()

	if provider.calls == 0 {
		t.Fatal("the queued wake never started a turn; the result is lost again")
	}
	if n := countEvents(sess, "followup_turn"); n != 1 {
		t.Errorf("expected exactly 1 followup_turn after delivery, got %d", n)
	}
	if !waitForAssistantMessage(t, sess, "deferred answer", 2*time.Second) {
		t.Fatalf("no answer from the delivered wake; messages=%+v", sess.SnapshotMessages())
	}
}

// TestFollowUpTurn_DedupesPerTask pins the idempotency set: one task resumes a
// session at most once, however many terminal events it publishes.
func TestFollowUpTurn_DedupesPerTask(t *testing.T) {
	h, store, _ := newFollowUpHandler(t, "one answer")
	store.GetOrCreate("s_dup")

	if !h.startFollowUpTurn("s_dup", "task-dup", "notice") {
		t.Fatal("first follow-up was refused")
	}
	h.wg.Wait()
	if h.startFollowUpTurn("s_dup", "task-dup", "notice") {
		t.Fatal("second follow-up for the same task must be refused")
	}

	// A different task is a different follow-up.
	if !h.startFollowUpTurn("s_dup", "task-other", "notice") {
		t.Error("a different task must still be able to resume the session")
	}
	h.wg.Wait()
}

// TestFollowUpTurn_DepthCapStopsRunawayChains pins the ceiling on chained
// follow-ups (a follow-up can delegate again, which can complete again).
func TestFollowUpTurn_DepthCapStopsRunawayChains(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "answer")
	store.GetOrCreate("s_deep")
	h.followUpDepth.Store("s_deep", maxFollowUpDepth)

	if h.startFollowUpTurn("s_deep", "task-deep", "notice") {
		t.Fatalf("follow-up started at depth %d, cap is %d", maxFollowUpDepth, maxFollowUpDepth)
	}
	if provider.calls != 0 {
		t.Errorf("provider called %d time(s) past the depth cap", provider.calls)
	}
}

// TestFollowUpTrigger_ForbidsRewritingTheDeliverable pins the wording. It is a
// design rule (deliver, do not take a second attempt at the work), added after
// the first forced end-to-end run read the sub-agent's report and wrote it back.
// The score difference that prompted it turned out to be model variance — see
// the constant's comment — so this test exists to keep the instruction from
// silently disappearing, not to protect a measured improvement.
func TestFollowUpTrigger_ForbidsRewritingTheDeliverable(t *testing.T) {
	msg := fmt.Sprintf(followUpTriggerFmt, "task-x")
	for _, want := range []string{
		"task-x",
		"has finished",
		"do not rewrite the artifacts it produced",
		"Only create a file if the task produced none",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("follow-up trigger must contain %q; got %q", want, msg)
		}
	}
}

// TestFollowUpTrigger_CarriesTheTaskOutcome pins that the notification text
// reaches the turn. startFollowUpTurn has taken a summary parameter since it was
// written and ignored it until 2026-11-06, so the turn was told to "read the
// artifacts it produced" while the task had produced none, and it answered the
// user's original request from its own priors (task_75f448f3).
func TestFollowUpTrigger_CarriesTheTaskOutcome(t *testing.T) {
	h, store, provider := newFollowUpHandler(t, "noted")
	store.GetOrCreate("s_trigger")

	notice := "[Task Notification] Delegated orchestrator task 'x' (task-abc) finished with gaps: " +
		"step s1 reported status=partial, confidence 0.30. No output files were produced."
	if !h.startFollowUpTurn("s_trigger", "task-abc", notice) {
		t.Fatal("follow-up turn was not started for an idle session")
	}
	h.wg.Wait()

	if provider.calls == 0 {
		t.Fatal("provider was never called")
	}
	if !strings.Contains(provider.lastIn, "finished with gaps") {
		t.Errorf("the follow-up turn was never told what the task actually produced; prompt was:\n%s", provider.lastIn)
	}
	if !strings.Contains(provider.lastIn, "do not rewrite the artifacts") {
		t.Errorf("the standing design rule must survive alongside the outcome:\n%s", provider.lastIn)
	}
}

// TestFollowUpTurn_NilHarnessIsSafe: a handler built without a harness (many
// tests, and any embedding that only uses slash commands) must not panic.
func TestFollowUpTurn_NilHarnessIsSafe(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	store.GetOrCreate("s_nil")
	h := &ChatHandler{Store: store}
	if h.startFollowUpTurn("s_nil", "task", "notice") {
		t.Fatal("follow-up started without a harness")
	}
	if h.startFollowUpTurn("", "task", "notice") {
		t.Fatal("follow-up started without a session id")
	}
}
