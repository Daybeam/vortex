package api

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/tools"
)

// TestChatHandler_ShutdownWaitsForGoroutines is the regression test for
// audit C4: ChatHandler must track in-flight goroutines via a WaitGroup so
// Shutdown() can wait for them to drain instead of orphaning LLM calls.
//
// This test would fail if the wg.Add/Done wrappers were removed from the
// goroutine launch sites in SendHandler — Shutdown would return immediately
// while the goroutine is still running.
func TestChatHandler_ShutdownWaitsForGoroutines(t *testing.T) {
	h := &ChatHandler{}

	goroutineDone := make(chan struct{})

	// Simulate a chat run goroutine (the real SendHandler does h.wg.Add(1)
	// before launching and defer h.wg.Done() inside).
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		time.Sleep(50 * time.Millisecond) // simulate LLM call
		close(goroutineDone)
	}()

	// Shutdown must block until the goroutine completes.
	shutdownReturned := make(chan struct{})
	go func() {
		h.Shutdown()
		close(shutdownReturned)
	}()

	select {
	case <-shutdownReturned:
		// Check that the goroutine actually finished (not a false positive
		// where Shutdown returned because wg was never incremented).
		select {
		case <-goroutineDone:
			// Success — Shutdown waited for the goroutine.
		default:
			t.Fatal("Shutdown returned but the tracked goroutine is still running")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return within 2s — goroutine not tracked by WaitGroup (audit C4)")
	}
}

func TestWireTaskCompletionCallbacks_InjectsNotificationIntoChatSession(t *testing.T) {
	chatStore := chat.NewChatSessionStore(t.TempDir(), nil)
	session := chatStore.GetOrCreate("s_test1")

	reg, err := config.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry failed: %v", err)
	}
	logger, err := obs.NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger failed: %v", err)
	}
	tmpDir := t.TempDir()
	engine := core.NewDirectedEngine(reg, nil, nil, nil, logger, nil, tmpDir, tmpDir, nil)

	// Register a task graph in engine
	graph := &schemas.TaskGraph{
		TaskID:    "task_100",
		SessionID: "s_test1",
		Status:    schemas.GraphCompleted,
		Steps:     map[string]*schemas.Step{},
	}
	engine.RegisterGraphForTest(graph)

	app := &tools.App{
		Scheduler: engine,
	}

	h := &ChatHandler{
		App:   app,
		Store: chatStore,
	}
	h.WireTaskCompletionCallbacks()

	// Publish EventTaskCompleted on DefaultBus
	obs.DefaultBus.Publish(obs.NewAgentEvent("task_100", "", string(obs.EventTaskCompleted), nil))

	// Allow goroutine/event bus callback to execute
	time.Sleep(50 * time.Millisecond)

	// Verify session events
	events := session.EventsAfter(-1)
	foundCallback := false
	for _, ev := range events {
		if ev.Type == "task_callback" {
			foundCallback = true
			if metaTaskID, ok := ev.Meta["task_id"].(string); !ok || metaTaskID != "task_100" {
				t.Errorf("expected meta task_id task_100, got %v", ev.Meta["task_id"])
			}
		}
	}
	if !foundCallback {
		t.Error("expected task_callback event injected into ChatSession, but none found")
	}

	// Verify session messages
	msgs := session.SnapshotMessages()
	foundMsg := false
	for _, m := range msgs {
		if m.Role == "assistant" && m.Content != "" {
			foundMsg = true
		}
	}
	if !foundMsg {
		t.Error("expected notification assistant message in ChatSession, but none found")
	}
}

// TestChatSubmitterIsAdmin_ChannelTierWins is a regression test for the
// 2026-10-22 security fix: a channel request's tier (req.UserTier) must decide
// admin-ness for the mount gate, NOT the context/app tier. Before the fix,
// ProcessMessage used EffectiveTier(ctx, app), which resolves to the app-level
// tier ("admin" for the main app) — so every Telegram guest/authorized user was
// treated as admin and could mount admin_only MCPs.
func TestChatSubmitterIsAdmin_ChannelTierWins(t *testing.T) {
	app := &tools.App{Tier: tools.TierAdmin} // main app runs in admin mode
	ctx := context.Background()              // Telegram never places tier in ctx

	if chatSubmitterIsAdmin(ctx, app, TierGuest) {
		t.Fatal("SECURITY: Telegram guest must NOT be treated as admin")
	}
	if chatSubmitterIsAdmin(ctx, app, TierAuthorized) {
		t.Fatal("SECURITY: Telegram authorized must NOT be treated as admin")
	}
	if !chatSubmitterIsAdmin(ctx, app, TierAdmin) {
		t.Fatal("Telegram admin must be treated as admin")
	}
	// No channel tier → fall back to the context/app tier (web/stdio path).
	if !chatSubmitterIsAdmin(ctx, app, "") {
		t.Fatal("empty channel tier must fall back to the (admin) app tier")
	}
	// A public app with no channel tier must not resolve to admin.
	if chatSubmitterIsAdmin(ctx, &tools.App{Tier: tools.TierPublic}, "") {
		t.Fatal("public app with no channel tier must not be admin")
	}
}
