package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/tools"
)

func TestHandleSlashCommand_Help(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/help", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /help to be handled")
	}
	if !strings.Contains(resp, "/help") || !strings.Contains(resp, "/tools") || !strings.Contains(resp, "/delegate") {
		t.Errorf("help text missing command names, got: %s", resp)
	}
}

func TestHandleSlashCommand_Tools(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/tools", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /tools to be handled")
	}
	// ToolDefinitions always includes delegate_to_orchestrator.
	if !strings.Contains(resp, "delegate_to_orchestrator") {
		t.Errorf("expected delegate_to_orchestrator in tools list, got: %s", resp)
	}
}

func TestHandleSlashCommand_DelegateEmptyArg(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/delegate", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /delegate with no arg to be handled")
	}
	if !strings.Contains(resp, "Usage:") {
		t.Errorf("expected usage hint, got: %s", resp)
	}
}

func TestHandleSlashCommand_DelegateNilFunc(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}} // Delegate is nil
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/delegate do something", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /delegate to be handled")
	}
	if !strings.Contains(resp, "not configured") {
		t.Errorf("expected 'not configured' error, got: %s", resp)
	}
}

func TestHandleSlashCommand_DelegateSuccess(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{
		Delegate: func(prompt string, _ int) (string, error) {
			return "task-123", nil
		},
	}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/delegate analyze data", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /delegate to be handled")
	}
	if !strings.Contains(resp, "task-123") {
		t.Errorf("expected task ID in response, got: %s", resp)
	}
}

// TestHandleSlashCommand_Clear_Step1_Challenge verifies that /clear alone
// issues a confirmation challenge and does NOT clear the session immediately.
// Regression for audit T-18-1: the two-step confirmation prevents accidental
// irreversible history loss. See AUDIT_REPORT_2026-10-18.md §T-18-1.
func TestHandleSlashCommand_Clear_Step1_Challenge(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	h := &ChatHandler{Harness: &chat.ChatHarness{}, Store: store}
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("m1", "hello", "", "")

	handled, resp := h.handleSlashCommand("/clear", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /clear to be handled")
	}
	// Step 1 must issue a challenge, not clear immediately.
	if !strings.Contains(resp, "IRREVERSIBLY") {
		t.Errorf("expected 'IRREVERSIBLY' warning in challenge, got: %s", resp)
	}
	if !strings.Contains(resp, "confirm") {
		t.Errorf("expected 'confirm' instruction in challenge, got: %s", resp)
	}
	// Messages must NOT be cleared at step 1.
	if len(session.Messages) != 1 {
		t.Errorf("expected 1 message after challenge (not cleared), got %d", len(session.Messages))
	}
}

// TestHandleSlashCommand_Clear_Step2_Confirm verifies that /clear <sessionID>
// confirm executes the clear after a prior /clear challenge.
func TestHandleSlashCommand_Clear_Step2_Confirm(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	h := &ChatHandler{Harness: &chat.ChatHarness{}, Store: store}
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("m1", "hello", "", "")

	// Step 1: issue challenge.
	h.handleSlashCommand("/clear", session, "s1", TierAdmin)
	// Step 2: confirm with correct session ID.
	handled, resp := h.handleSlashCommand("/clear s1 confirm", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /clear s1 confirm to be handled")
	}
	if !strings.Contains(resp, "cleared") {
		t.Errorf("expected 'cleared' in response, got: %s", resp)
	}
	if len(session.Messages) != 0 {
		t.Errorf("expected 0 messages after confirm, got %d", len(session.Messages))
	}
}

// TestHandleSlashCommand_Clear_WrongSession verifies that /clear with a
// mismatched session ID is rejected (prevents clearing another session).
func TestHandleSlashCommand_Clear_WrongSession(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	h := &ChatHandler{Harness: &chat.ChatHarness{}, Store: store}
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("m1", "hello", "", "")

	h.handleSlashCommand("/clear", session, "s1", TierAdmin)
	handled, resp := h.handleSlashCommand("/clear wrong-session confirm", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /clear wrong-session confirm to be handled")
	}
	if !strings.Contains(resp, "cancelled") {
		t.Errorf("expected 'cancelled' for wrong session ID, got: %s", resp)
	}
	if len(session.Messages) != 1 {
		t.Errorf("expected 1 message (not cleared on wrong session), got %d", len(session.Messages))
	}
}

// TestHandleSlashCommand_Clear_NoPending verifies that /clear <sessionID>
// confirm without a prior /clear challenge is rejected.
func TestHandleSlashCommand_Clear_NoPending(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	h := &ChatHandler{Harness: &chat.ChatHarness{}, Store: store}
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("m1", "hello", "", "")

	handled, resp := h.handleSlashCommand("/clear s1 confirm", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /clear s1 confirm to be handled")
	}
	if !strings.Contains(resp, "No pending confirmation") {
		t.Errorf("expected 'No pending confirmation', got: %s", resp)
	}
	if len(session.Messages) != 1 {
		t.Errorf("expected 1 message (not cleared without challenge), got %d", len(session.Messages))
	}
}

// TestHandleSlashCommand_Clear_Expired verifies that an expired confirmation
// token is rejected (30-second expiry window).
func TestHandleSlashCommand_Clear_Expired(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	h := &ChatHandler{Harness: &chat.ChatHarness{}, Store: store}
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("m1", "hello", "", "")

	// Store an already-expired deadline (current time - 1 second).
	h.clearConfirmations.Store("s1", time.Now().Unix()-1)

	handled, resp := h.handleSlashCommand("/clear s1 confirm", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /clear s1 confirm to be handled")
	}
	if !strings.Contains(resp, "expired") {
		t.Errorf("expected 'expired' for stale confirmation, got: %s", resp)
	}
	if len(session.Messages) != 1 {
		t.Errorf("expected 1 message (not cleared on expired token), got %d", len(session.Messages))
	}
}

// TestHandleSlashCommand_Clear_LastThinkingRegression is a regression test for
// the lastThinking memory leak (audit MEDIUM 2026-10-28). The lastThinking
// sync.Map had Store per sessionID but no Delete on /clear, causing unbounded
// memory growth. This test verifies that /clear also removes the lastThinking entry.
func TestHandleSlashCommand_Clear_LastThinkingRegression(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	h := &ChatHandler{Harness: &chat.ChatHarness{}, Store: store}
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("m1", "hello", "", "")

	// Store a lastThinking entry for the session.
	h.StoreLastThinking("s1", "some reasoning content")
	if _, ok := h.lastThinking.Load("s1"); !ok {
		t.Fatal("lastThinking should contain the session after StoreLastThinking")
	}

	// Step 1: issue challenge. Step 2: confirm.
	h.handleSlashCommand("/clear", session, "s1", TierAdmin)
	h.handleSlashCommand("/clear s1 confirm", session, "s1", TierAdmin)

	// Verify the lastThinking entry is gone.
	if _, ok := h.lastThinking.Load("s1"); ok {
		t.Error("lastThinking should NOT contain the session after /clear — memory leak regression")
	}
}

func TestHandleSlashCommand_UnknownCommand(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, _ := h.handleSlashCommand("/foobar", session, "s1", TierAdmin)
	if handled {
		t.Fatal("expected unknown slash command to NOT be handled (fall through to LLM)")
	}
}

func TestHandleSlashCommand_NonSlashInput(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, _ := h.handleSlashCommand("hello world", session, "s1", TierAdmin)
	if handled {
		t.Fatal("expected non-slash input to NOT be handled")
	}
}

func TestHandleSlashCommand_StopNoActiveRun(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/stop", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /stop to be handled")
	}
	if !strings.Contains(resp, "No active run") {
		t.Errorf("expected 'No active run' message, got: %s", resp)
	}
}

func TestHandleSlashCommand_StopCancelsActiveRun(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	cancelled := make(chan struct{})
	h.cancelFuncs.Store("s1", context.CancelFunc(func() {
		close(cancelled)
	}))

	handled, resp := h.handleSlashCommand("/stop", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /stop to be handled")
	}
	if !strings.Contains(resp, "Stopped") {
		t.Errorf("expected 'Stopped' message, got: %s", resp)
	}
	select {
	case <-cancelled:
		// Success — cancel func was invoked.
	default:
		t.Fatal("expected cancel func to be invoked")
	}
	// Verify the cancel func was removed from the map.
	if _, ok := h.cancelFuncs.Load("s1"); ok {
		t.Fatal("expected cancel func to be removed from map after /stop")
	}
}

func TestHandleSlashCommand_HelpIncludesNewCommands(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/help", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /help to be handled")
	}
	expected := []string{"/tasks", "/status", "/cancel", "/logs", "/models", "/skills", "/schedule", "/discover", "/config", "/stop", "/thinking", "/pending", "/decision", "/approve"}
	for _, cmd := range expected {
		if !strings.Contains(resp, cmd) {
			t.Errorf("help text missing %s, got: %s", cmd, resp)
		}
	}
}

func TestHandleSlashCommand_ThinkingNoContent(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/thinking", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /thinking to be handled")
	}
	if !strings.Contains(resp, "No thinking") {
		t.Errorf("expected 'No thinking' message, got: %s", resp)
	}
}

func TestHandleSlashCommand_ThinkingWithContent(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	h.StoreLastThinking("s1", "This is my reasoning process...")
	handled, resp := h.handleSlashCommand("/thinking", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /thinking to be handled")
	}
	if !strings.Contains(resp, "This is my reasoning") {
		t.Errorf("expected thinking content in response, got: %s", resp)
	}
}

func TestHandleSlashCommand_DecisionUsage(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/decision", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /decision to be handled")
	}
	if !strings.Contains(resp, "Usage:") {
		t.Errorf("expected usage hint, got: %s", resp)
	}
}

func TestHandleSlashCommand_ApproveUsage(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/approve", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /approve to be handled")
	}
	if !strings.Contains(resp, "Usage:") {
		t.Errorf("expected usage hint, got: %s", resp)
	}
}

func TestHandleSlashCommand_PendingNoArg(t *testing.T) {
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	handled, resp := h.handleSlashCommand("/pending", session, "s1", TierAdmin)
	if !handled {
		t.Fatal("expected /pending to be handled")
	}
	if !strings.Contains(resp, "not available") {
		t.Errorf("expected 'not available' error with nil App, got: %s", resp)
	}
}

func TestHandleSlashCommand_NilAppGracefulDegradation(t *testing.T) {
	// All new commands should return a graceful error when App is nil
	// instead of panicking.
	h := &ChatHandler{Harness: &chat.ChatHarness{}}
	session := &chat.ChatSession{ID: "s1", Messages: make(map[string]*chat.ChatMessage)}

	commands := []string{"/tasks", "/status foo", "/cancel foo", "/logs foo", "/models", "/skills", "/schedule", "/discover", "/config", "/pending", "/pending foo", "/decision a b c", "/approve a b"}
	for _, cmd := range commands {
		handled, resp := h.handleSlashCommand(cmd, session, "s1", TierAdmin)
		if !handled {
			t.Errorf("expected %s to be handled even with nil App", cmd)
		}
		if !strings.Contains(resp, "not available") {
			t.Errorf("expected 'not available' error for %s with nil App, got: %s", cmd, resp)
		}
	}
}

// Ensure cancelFuncs is a sync.Map (compile-time check).
var _ sync.Map

// TestHumanApprovalDecisionDenied_PageOnly is a regression test for the
// 2026-10-22 security fix: human_approval_required decisions must only be
// approvable via a web-page click. /decision and /approve previously accepted
// any decision (from any Telegram user), defeating that intent.
func TestHumanApprovalDecisionDenied_PageOnly(t *testing.T) {
	engine := newTestEngine(t)
	engine.RegisterGraphForTest(&schemas.TaskGraph{
		TaskID: "t-approval",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{
			{ID: "d_approve", StepID: "s1", Type: schemas.DecisionHumanApprovalRequired},
			{ID: "d_other", StepID: "s2", Type: schemas.DecisionStepFailed},
		},
	})
	h := &ChatHandler{App: &tools.App{Scheduler: engine}}

	if !h.humanApprovalDecisionDenied("t-approval", "d_approve") {
		t.Fatal("SECURITY: human_approval_required must be denied on chat (page-only)")
	}
	if h.humanApprovalDecisionDenied("t-approval", "d_other") {
		t.Fatal("a non-approval decision must NOT be denied on chat")
	}
	if h.humanApprovalDecisionDenied("t-approval", "d_missing") {
		t.Fatal("an unknown decision id must not be denied")
	}
}
