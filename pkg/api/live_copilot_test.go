package api

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/tools"
)

// ── Live Co-pilot Shared Session Tests ──────────────────────────────────────
// See docs/plan/LIVE_COPILOT_SHARED_SESSION_DESIGN.md §4.

// TestLiveCopilot_StartSharedSession verifies that /live start marks the
// session as shared and sets the OwnerID to the caller's ID.
func TestLiveCopilot_StartSharedSession(t *testing.T) {
	h := newTestChatHandler(t)
	session := h.Store.GetOrCreate("sess-live-start")

	handled, resp := h.cmdLive("/live start", session, "sess-live-start", TierAdmin, "admin-user-42")
	if !handled {
		t.Fatal("expected /live start to be handled")
	}
	if !strings.Contains(resp, "shared") {
		t.Errorf("expected 'shared' in response, got: %s", resp)
	}
	if !session.IsShared() {
		t.Error("session should be marked as shared after /live start")
	}
	if session.GetOwnerID() != "admin-user-42" {
		t.Errorf("expected OwnerID 'admin-user-42', got %q", session.GetOwnerID())
	}
}

// TestLiveCopilot_JoinSharedSession verifies that /live join <id> looks up
// a shared session and returns its info.
func TestLiveCopilot_JoinSharedSession(t *testing.T) {
	h := newTestChatHandler(t)

	// Create and mark a session as shared.
	shared := h.Store.GetOrCreate("sess-shared-123")
	shared.SetShared(true)
	shared.SetOwnerID("owner-abc")

	// Join from a different session.
	current := h.Store.GetOrCreate("sess-current")
	handled, resp := h.cmdLive("/live join sess-shared-123", current, "sess-current", TierGuest, "guest-1")
	if !handled {
		t.Fatal("expected /live join to be handled")
	}
	if !strings.Contains(resp, "sess-shared-123") {
		t.Errorf("expected target session ID in response, got: %s", resp)
	}
	if !strings.Contains(resp, "owner-abc") {
		t.Errorf("expected owner ID in response, got: %s", resp)
	}
}

// TestLiveCopilot_JoinNonSharedSession verifies that /live join on a
// non-shared session returns an appropriate error.
func TestLiveCopilot_JoinNonSharedSession(t *testing.T) {
	h := newTestChatHandler(t)
	h.Store.GetOrCreate("sess-not-shared") // exists but not shared

	current := h.Store.GetOrCreate("sess-current")
	handled, resp := h.cmdLive("/live join sess-not-shared", current, "sess-current", TierGuest, "guest-1")
	if !handled {
		t.Fatal("expected /live join to be handled")
	}
	if !strings.Contains(resp, "not shared") {
		t.Errorf("expected 'not shared' error, got: %s", resp)
	}
}

// TestLiveCopilot_EndSharedSession verifies that /live end unmarks the
// session as shared (admin only).
func TestLiveCopilot_EndSharedSession(t *testing.T) {
	h := newTestChatHandler(t)
	session := h.Store.GetOrCreate("sess-live-end")
	session.SetShared(true)
	session.SetOwnerID("admin-1")

	handled, resp := h.cmdLive("/live end", session, "sess-live-end", TierAdmin, "admin-1")
	if !handled {
		t.Fatal("expected /live end to be handled")
	}
	if !strings.Contains(resp, "ended") {
		t.Errorf("expected 'ended' in response, got: %s", resp)
	}
	if session.IsShared() {
		t.Error("session should not be shared after /live end")
	}
}

// TestLiveCopilot_Status verifies that /live status shows shared session info.
func TestLiveCopilot_Status(t *testing.T) {
	h := newTestChatHandler(t)
	session := h.Store.GetOrCreate("sess-live-status")
	session.SetShared(true)
	session.SetOwnerID("admin-status-owner")

	handled, resp := h.cmdLive("/live status", session, "sess-live-status", TierGuest, "guest-1")
	if !handled {
		t.Fatal("expected /live status to be handled")
	}
	if !strings.Contains(resp, "sess-live-status") {
		t.Errorf("expected session ID in response, got: %s", resp)
	}
	if !strings.Contains(resp, "admin-status-owner") {
		t.Errorf("expected owner ID in response, got: %s", resp)
	}
}

// TestLiveCopilot_StatusNotShared verifies /live status on a non-shared session.
func TestLiveCopilot_StatusNotShared(t *testing.T) {
	h := newTestChatHandler(t)
	session := h.Store.GetOrCreate("sess-not-shared-status")

	handled, resp := h.cmdLive("/live status", session, "sess-not-shared-status", TierGuest, "guest-1")
	if !handled {
		t.Fatal("expected /live status to be handled")
	}
	if !strings.Contains(resp, "not shared") {
		t.Errorf("expected 'not shared' in response, got: %s", resp)
	}
}

// TestLiveCopilot_GuestCannotStart verifies that non-admin users cannot
// run /live start.
func TestLiveCopilot_GuestCannotStart(t *testing.T) {
	h := newTestChatHandler(t)
	session := h.Store.GetOrCreate("sess-guest-start")

	handled, resp := h.cmdLive("/live start", session, "sess-guest-start", TierGuest, "guest-1")
	if !handled {
		t.Fatal("expected /live start to be handled")
	}
	if !strings.Contains(resp, "admin") {
		t.Errorf("expected 'admin' in rejection message, got: %s", resp)
	}
	if session.IsShared() {
		t.Error("session should not be shared after rejected /live start")
	}
}

// TestLiveCopilot_GuestCannotEnd verifies that non-admin users cannot
// run /live end.
func TestLiveCopilot_GuestCannotEnd(t *testing.T) {
	h := newTestChatHandler(t)
	session := h.Store.GetOrCreate("sess-guest-end")
	session.SetShared(true)
	session.SetOwnerID("admin-1")

	handled, resp := h.cmdLive("/live end", session, "sess-guest-end", TierGuest, "guest-1")
	if !handled {
		t.Fatal("expected /live end to be handled")
	}
	if !strings.Contains(resp, "admin") {
		t.Errorf("expected 'admin' in rejection message, got: %s", resp)
	}
	if !session.IsShared() {
		t.Error("session should still be shared after rejected /live end")
	}
}

// TestLiveCopilot_DefaultUsage verifies that /live with no subcommand
// returns usage instructions.
func TestLiveCopilot_DefaultUsage(t *testing.T) {
	h := newTestChatHandler(t)
	session := h.Store.GetOrCreate("sess-live-usage")

	handled, resp := h.cmdLive("/live", session, "sess-live-usage", TierGuest, "guest-1")
	if !handled {
		t.Fatal("expected /live to be handled")
	}
	if !strings.Contains(resp, "Usage:") {
		t.Errorf("expected usage instructions, got: %s", resp)
	}
}

// TestLiveCopilot_NonSharedSessionUnaffected verifies backward compatibility:
// a non-shared session processes messages normally without AuthOwnerKey override.
func TestLiveCopilot_NonSharedSessionUnaffected(t *testing.T) {
	h := newTestChatHandler(t)

	// Create a non-shared session (default state).
	session := h.Store.GetOrCreate("sess-normal")
	if session.IsShared() {
		t.Fatal("new session should not be shared by default")
	}

	// Process a slash command — should work normally.
	resp := h.ProcessMessage(context.Background(), ChatRequest{
		Content:   "/help",
		SessionID: "sess-normal",
		MessageID: "msg-1",
		UserTier:  TierGuest,
	})
	if !resp.SlashCommand {
		t.Fatal("expected /help to be handled as slash command")
	}
}

// TestLiveCopilot_GuestOnSharedSession verifies that a guest sending a
// message to a shared session gets processed (AuthOwnerKey override happens
// without error, even though no provider is configured).
func TestLiveCopilot_GuestOnSharedSession(t *testing.T) {
	h := newTestChatHandler(t)

	// Wire a registry so role selection doesn't nil-deref.
	reg, err := config.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	h.App.Registry = reg
	// Set an empty harness so h.Harness.Provider == nil triggers the early
	// return ("no provider configured") without nil-derefing h.Harness itself.
	h.Harness = &chat.ChatHarness{}

	// Create and mark a session as shared.
	session := h.Store.GetOrCreate("sess-shared-guest")
	session.SetShared(true)
	session.SetOwnerID("admin-owner-123")

	// Guest sends a message — the AuthOwnerKey override should happen
	// before the provider check. The message will fail with "no provider
	// configured" but the override should not cause any error.
	ctx := context.WithValue(context.Background(), tools.AuthOwnerKey, "guest-original-id")
	resp := h.ProcessMessage(ctx, ChatRequest{
		Content:   "hello from guest",
		SessionID: "sess-shared-guest",
		MessageID: "msg-guest-1",
		UserTier:  TierGuest,
	})

	// The response should not be an error about AuthOwnerKey or shared sessions.
	// It may be "no provider configured" which is expected in test setup.
	if resp.Error != "" && strings.Contains(resp.Error, "shared") {
		t.Errorf("unexpected error related to shared session: %s", resp.Error)
	}
	if resp.Error != "" && strings.Contains(resp.Error, "AuthOwnerKey") {
		t.Errorf("unexpected error related to AuthOwnerKey: %s", resp.Error)
	}
}

// TestLiveCopilot_ChatSessionFields verifies the thread-safe getter/setter
// methods for the new ChatSession fields.
func TestLiveCopilot_ChatSessionFields(t *testing.T) {
	s := &chat.ChatSession{}

	// Default values.
	if s.IsShared() {
		t.Error("new session should not be shared by default")
	}
	if s.GetOwnerID() != "" {
		t.Error("new session should have empty OwnerID by default")
	}

	// Set shared.
	s.SetShared(true)
	if !s.IsShared() {
		t.Error("IsShared should return true after SetShared(true)")
	}
	s.SetShared(false)
	if s.IsShared() {
		t.Error("IsShared should return false after SetShared(false)")
	}

	// Set OwnerID.
	s.SetOwnerID("owner-123")
	if s.GetOwnerID() != "owner-123" {
		t.Errorf("GetOwnerID should return 'owner-123', got %q", s.GetOwnerID())
	}
}
