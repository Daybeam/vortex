package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

// These tests spawn the real scripts/jit/repl_server.py subprocess (not
// mocked) -- per the design doc's own §5 validation order ("POC 落地前，
// 不写全部代码就先验证的部分"), this is the point: prove state genuinely
// persists across calls against a real Python process, not a mock that
// could hide a wiring bug. Requires "python" resolvable on PATH -- if this
// suite is skipped in some CI environment lacking Python, that's a real
// gap worth noting, not silently working around it with mocks.

func newTestJITSessionManager(t *testing.T) *JITSessionManager {
	t.Helper()
	reg := &config.Registry{}
	// core/ is this package's own directory; scripts/ lives one level up at
	// the project root, matching NewJITSessionManager's documented
	// scriptsDir convention (same as JITManager's workDir param elsewhere).
	return NewJITSessionManager(reg, "../scripts")
}

func TestJITSession_StatePersistsAcrossEvalCalls(t *testing.T) {
	m := newTestJITSessionManager(t)
	defer m.CloseSession("t1")

	sess, err := m.GetOrCreateSession("t1", "python", 30*time.Second)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := sess.Eval(ctx, "x = 1 + 1"); err != nil {
		t.Fatalf("first Eval: %v", err)
	}

	res, err := sess.Eval(ctx, "__result__ = x")
	if err != nil {
		t.Fatalf("second Eval: %v", err)
	}
	// JSON numbers decode as float64 in Go's encoding/json.
	if f, ok := res.Result.(float64); !ok || f != 2 {
		t.Errorf("expected persisted x==2 from the first call, got %#v (this is the core claim—state must survive across separate Eval calls on the same session)", res.Result)
	}
}

func TestJITSession_ErrorDoesNotKillSession(t *testing.T) {
	m := newTestJITSessionManager(t)
	defer m.CloseSession("t2")

	sess, err := m.GetOrCreateSession("t2", "python", 30*time.Second)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := sess.Eval(ctx, "1/0")
	if err != nil {
		t.Fatalf("Eval with a Python exception should not surface as a Go-level error, got: %v", err)
	}
	if !strings.Contains(res.Stderr, "division by zero") {
		t.Errorf("expected the exception text in Stderr, got %q", res.Stderr)
	}

	// The session must still be usable after an exception -- the process
	// wasn't killed, only that one eval call reported an error.
	res2, err := sess.Eval(ctx, "__result__ = 42")
	if err != nil {
		t.Fatalf("Eval after a prior exception: %v", err)
	}
	if f, ok := res2.Result.(float64); !ok || f != 42 {
		t.Errorf("expected 42 from a fresh eval after the exception, got %#v", res2.Result)
	}
}

func TestJITSessionManager_GetOrCreateSession_ReusesSameSession(t *testing.T) {
	m := newTestJITSessionManager(t)
	defer m.CloseSession("t3")

	s1, err := m.GetOrCreateSession("t3", "python", 30*time.Second)
	if err != nil {
		t.Fatalf("first GetOrCreateSession: %v", err)
	}
	s2, err := m.GetOrCreateSession("t3", "python", 30*time.Second)
	if err != nil {
		t.Fatalf("second GetOrCreateSession: %v", err)
	}
	if s1 != s2 {
		t.Errorf("expected the same *JITSession to be returned for the same sessionID, got two different sessions")
	}
	if got := m.SessionCount(); got != 1 {
		t.Errorf("expected exactly 1 tracked session, got %d", got)
	}
}

func TestJITSessionManager_CloseSession_SpawnsGenuinelyFreshProcessOnRecreate(t *testing.T) {
	m := newTestJITSessionManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := m.GetOrCreateSession("t4", "python", 30*time.Second)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	if _, err := sess.Eval(ctx, "x = 99"); err != nil {
		t.Fatalf("Eval: %v", err)
	}

	m.CloseSession("t4")
	if got := m.SessionCount(); got != 0 {
		t.Fatalf("expected 0 sessions after CloseSession, got %d", got)
	}

	// Recreating under the same sessionID must be a genuinely new process
	// with no memory of the old one's globals -- NameError, not 99.
	sess2, err := m.GetOrCreateSession("t4", "python", 30*time.Second)
	if err != nil {
		t.Fatalf("GetOrCreateSession after close: %v", err)
	}
	defer m.CloseSession("t4")

	res, err := sess2.Eval(ctx, "__result__ = x")
	if err != nil {
		t.Fatalf("Eval on the recreated session: %v", err)
	}
	if !strings.Contains(res.Stderr, "not defined") {
		t.Errorf("expected a NameError-shaped message proving the recreated session has no memory of the closed one's state, got stderr=%q result=%#v", res.Stderr, res.Result)
	}
}

func TestJITSessionManager_TTLExpiry_AutoCloses(t *testing.T) {
	m := newTestJITSessionManager(t)

	_, err := m.GetOrCreateSession("t5", "python", 300*time.Millisecond)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	if got := m.SessionCount(); got != 1 {
		t.Fatalf("expected 1 session right after creation, got %d", got)
	}

	// Poll rather than a single fixed sleep, to avoid a flaky exact-timing
	// assumption about the TTL goroutine's scheduling.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.SessionCount() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("expected the session to be auto-closed by its TTL within 3s, but SessionCount is still %d", m.SessionCount())
}

func TestJITSessionManager_UnsupportedLang_ReturnsErrorNotPanic(t *testing.T) {
	m := newTestJITSessionManager(t)
	_, err := m.GetOrCreateSession("t6", "lua", 30*time.Second)
	if err == nil {
		t.Errorf("expected an error for lang=lua (not implemented in this POC), got nil")
	}
}

func TestJITSessionManager_Ping(t *testing.T) {
	m := newTestJITSessionManager(t)
	defer m.CloseSession("t7")

	sess, err := m.GetOrCreateSession("t7", "python", 30*time.Second)
	if err != nil {
		t.Fatalf("GetOrCreateSession: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	raw, err := sess.cli.SendRequest(ctx, "ping", map[string]any{})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	m2, ok := raw.(map[string]any)
	if !ok || m2["ok"] != true {
		t.Errorf("expected {ok:true} from ping, got %#v", raw)
	}
}

func TestJITSessionManager_GetOrCreateSessionForStep_DerivesConsistentKey(t *testing.T) {
	m := newTestJITSessionManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := m.GetOrCreateSessionForStep("task1", "step1", "python", 30*time.Second)
	if err != nil {
		t.Fatalf("GetOrCreateSessionForStep: %v", err)
	}
	if _, err := sess.Eval(ctx, "__result__ = 7"); err != nil {
		t.Fatalf("Eval: %v", err)
	}

	// A second call with the SAME (taskID, stepID) must reuse the same
	// session -- this is the key property CloseSessionsForStep and the
	// jit.eval action both depend on for correctness.
	sess2, err := m.GetOrCreateSessionForStep("task1", "step1", "python", 30*time.Second)
	if err != nil {
		t.Fatalf("second GetOrCreateSessionForStep: %v", err)
	}
	if sess != sess2 {
		t.Errorf("expected the same session for the same (taskID, stepID)")
	}

	// A different stepID under the same task must NOT share state.
	sessOther, err := m.GetOrCreateSessionForStep("task1", "step2", "python", 30*time.Second)
	if err != nil {
		t.Fatalf("GetOrCreateSessionForStep (different step): %v", err)
	}
	defer m.CloseSessionsForStep("task1", "step2")
	res, err := sessOther.Eval(ctx, "import sys; __result__ = 'x' in dir()")
	if err != nil {
		t.Fatalf("Eval on the other step's session: %v", err)
	}
	if res.Result != false {
		t.Errorf("expected a different (taskID, stepID) to get a fresh session with no leaked state, got %#v", res.Result)
	}

	m.CloseSessionsForStep("task1", "step1")
}

func TestJITSessionManager_CloseSessionsForStep_ClosesExactlyThatStepsSession(t *testing.T) {
	m := newTestJITSessionManager(t)

	if _, err := m.GetOrCreateSessionForStep("taskA", "step1", "python", 30*time.Second); err != nil {
		t.Fatalf("GetOrCreateSessionForStep: %v", err)
	}
	if _, err := m.GetOrCreateSessionForStep("taskA", "step2", "python", 30*time.Second); err != nil {
		t.Fatalf("GetOrCreateSessionForStep: %v", err)
	}
	defer m.CloseSessionsForStep("taskA", "step2")

	if got := m.SessionCount(); got != 2 {
		t.Fatalf("expected 2 sessions before closing, got %d", got)
	}

	m.CloseSessionsForStep("taskA", "step1")

	if got := m.SessionCount(); got != 1 {
		t.Errorf("expected exactly 1 session to remain after closing step1's, got %d", got)
	}
}

func TestJITSessionManager_CloseSessionsForStep_NoopWhenNoSessionExists(t *testing.T) {
	m := newTestJITSessionManager(t)
	// Must not panic or error when there was never a session for this step.
	m.CloseSessionsForStep("nonexistent-task", "nonexistent-step")
	if got := m.SessionCount(); got != 0 {
		t.Errorf("expected 0 sessions, got %d", got)
	}
}
