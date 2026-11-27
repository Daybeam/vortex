package api

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/chat"
)

// Regression tests for the "a chat turn must always reach a terminal event"
// invariant.
//
// Field failure (2026-10-30, harness-bench p3-d3d5 / task 026): the model
// finished at t=22 s, the JSON skeletonizer panicked inside ChatHarness.Run,
// the panic was recovered and printed to stderr and nothing else happened.
// StreamHandler only exits on a "done"/"error" event, so the SSE stream stayed
// open and the harness burned the rest of its 1200 s round before scoring the
// task as model_error. Two things were wrong: the panic itself (fixed in
// core/skeleton), and the silent recovery (fixed here).

// TestFinishTurn_PanicEmitsTerminalError covers the panic path.
func TestFinishTurn_PanicEmitsTerminalError(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	sess := store.GetOrCreate("s_panic")
	h := &ChatHandler{Store: store}

	h.finishTurn(sess, false, "index out of range [-1]")

	events := sess.EventsAfter(0)
	if len(events) != 1 {
		t.Fatalf("want exactly 1 event, got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Type != "error" {
		t.Fatalf("want terminal event type %q (StreamHandler only exits on done/error), got %q", "error", ev.Type)
	}
	if !strings.Contains(ev.Data, "internal error") {
		t.Errorf("event should name the failure mode, got %q", ev.Data)
	}
	if ev.Meta == nil || ev.Meta["panic"] != true {
		t.Errorf("want meta panic=true so the failure is distinguishable from a model error, got %+v", ev.Meta)
	}
}

// TestFinishTurn_SkipsWhenTerminalAlreadySent ensures the guard does not
// append a second terminal event after a normal turn.
func TestFinishTurn_SkipsWhenTerminalAlreadySent(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	sess := store.GetOrCreate("s_ok")
	h := &ChatHandler{Store: store}

	sess.AddEvent(chat.ChatEvent{Type: "done"})
	before := len(sess.EventsAfter(0))

	h.finishTurn(sess, true, nil)

	if after := len(sess.EventsAfter(0)); after != before {
		t.Fatalf("finishTurn appended %d event(s) after a terminal event", after-before)
	}
}

// TestFinishTurn_NoTerminalNoPanic covers an abnormal exit without a panic
// (e.g. a future early return added without emitting "done").
func TestFinishTurn_NoTerminalNoPanic(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	sess := store.GetOrCreate("s_silent")
	h := &ChatHandler{Store: store}

	h.finishTurn(sess, false, nil)

	events := sess.EventsAfter(0)
	if len(events) != 1 || events[0].Type != "error" {
		t.Fatalf("want a single terminal error event, got %+v", events)
	}
	if events[0].Meta == nil || events[0].Meta["no_terminal"] != true {
		t.Errorf("want meta no_terminal=true, got %+v", events[0].Meta)
	}
}

// TestFinishTurn_NilSession_NoPanic makes sure the containment path cannot
// itself panic while handling a panic (that would take down the process).
func TestFinishTurn_NilSession_NoPanic(t *testing.T) {
	h := &ChatHandler{}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("finishTurn panicked on nil session: %v", r)
		}
	}()
	h.finishTurn(nil, false, "boom")
}

// TestStreamHandler_ReturnsAfterFinishTurnError is the end-to-end assertion
// that matters: the SSE handler must actually terminate once the terminal
// event lands. Without this, "we emit an error event" could be true while the
// stream still hangs.
func TestStreamHandler_ReturnsAfterFinishTurnError(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	sess := store.GetOrCreate("s_stream")
	h := &ChatHandler{Store: store}

	h.finishTurn(sess, false, "index out of range [-1]")

	req := httptest.NewRequest("GET", "/api/chat/stream?session_id=s_stream", nil)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.StreamHandler(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("StreamHandler did not return after a terminal event — the SSE stream would hang until the client's deadline")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("stream body missing terminal error event, got:\n%s", body)
	}
}

// TestSendHandler_PanicPathWiredToFinishTurn guards the wiring, not the
// behaviour: finishTurn is only useful if the turn goroutine's defer actually
// calls it with recover(). Deleting that call would leave every test above
// green while the field hang returns.
func TestSendHandler_PanicPathWiredToFinishTurn(t *testing.T) {
	src, err := os.ReadFile("chat_handlers.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if !strings.Contains(string(src), "h.finishTurn(session, terminalSent, recover())") {
		t.Fatal("chat turn goroutine no longer calls finishTurn(session, terminalSent, recover()); a panic would hang the SSE stream again")
	}
	if !strings.Contains(string(src), "terminalSent = true") {
		t.Fatal("terminalSent is never set, so finishTurn would append a spurious error after every normal turn")
	}
}
