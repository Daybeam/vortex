package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// newTestEngine builds a real DirectedEngine whose NewLogger target dir exists.
// NB: obs.NewLogger("", nil) fails with "mkdir : no such file or directory"
// because the empty dir is passed to os.MkdirAll("") — use t.TempDir() instead.
func newTestEngine(t *testing.T) *core.DirectedEngine {
	t.Helper()
	reg, err := config.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logger, err := obs.NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	return core.NewDirectedEngine(reg, nil, nil, nil, logger, nil, t.TempDir(), t.TempDir(), nil)
}

// TestSubscribeTaskEvents_FiresForChatlessTask exercises the wiring that unit
// tests cannot reach: the EventBus subscription, the scheduler enrichment and
// fire-and-forget delivery for BOTH terminal events.
//
// The task graph is registered WITHOUT a chat SessionID on purpose. That is the
// regression this wiring exists for: handleTaskCompletion returns early for
// tasks with no chat session, so a webhook hook wired to run inside it would
// silently swallow every callback fired by a non-chat task (CLI, cron, API
// caller). Those are exactly the tasks that exist in the default stdio/MCP
// build, which has no chat surface at all.
func TestSubscribeTaskEvents_FiresForChatlessTask(t *testing.T) {
	const secret = "«REDACTED»"
	deliveries := make(chan []byte, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/webhooks/callback-notify" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("X-GitHub-Delivery") == "" {
			t.Error("X-GitHub-Delivery missing — the receiver needs it as an idempotency key")
		}
		if r.Header.Get("X-Hub-Signature-256") != signedHeaderFor(r, secret) {
			t.Error("HMAC signature rejected")
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		deliveries <- b
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"delivered"}`))
	}))
	defer srv.Close()

	engine := newTestEngine(t)
	engine.RegisterGraphForTest(&schemas.TaskGraph{
		TaskID:    "chatless-task-1",
		SessionID: "", // <-- no chat session on purpose
		Status:    schemas.GraphCompleted,
		Steps:     map[string]*schemas.Step{},
	})

	n := NewWebhookNotifier(srv.URL)
	n.secret = secret
	n.SubscribeTaskEvents(engine)
	defer n.UnsubscribeTaskEvents()

	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"chatless-task-1", "", string(obs.EventTaskCompleted), map[string]any{"confidence": 0.83}))
	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"chatless-task-1", "", string(obs.EventTaskFailed), map[string]any{"error": "boom"}))

	var bodies []byte
	for i := 0; i < 2; i++ {
		b := <-deliveries
		bodies = append(bodies, b...)
		bodies = append(bodies, '\n')
	}
	if !bytes.Contains(bodies, []byte(`"status":"completed"`)) {
		t.Errorf("missing task_completed delivery: %s", bodies)
	}
	if !bytes.Contains(bodies, []byte(`"status":"failed"`)) {
		t.Errorf("missing task_failed delivery: %s", bodies)
	}
	if !bytes.Contains(bodies, []byte(`"event_type":"task_failed"`)) {
		t.Errorf("failed delivery missing event_type: %s", bodies)
	}
	// The scheduler enrichment must have passed the payload through.
	if !bytes.Contains(bodies, []byte("confidence")) {
		t.Errorf("scheduler payload was dropped: %s", bodies)
	}
	if !bytes.Contains(bodies, []byte(`"task_id":"chatless-task-1"`)) {
		t.Errorf("task_id missing: %s", bodies)
	}
}

// TestWireTaskWebhook_DefaultOff covers the env-driven path used by main():
// unset URL → nil with no subscription; set URL → a working subscription that
// delivers and can be torn down.
func TestWireTaskWebhook_DefaultOff(t *testing.T) {
	deliveries := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		deliveries <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	engine := newTestEngine(t)

	// Default-off: URL unset → nil.
	if got := WireTaskWebhook(engine, nil); got != nil {
		t.Fatal("WireTaskWebhook without URL returned non-nil, want nil")
	}
	// Also off when the scheduler is absent.
	t.Setenv("WEBHOOK_CALLBACK_URL", srv.URL)
	if got := WireTaskWebhook(nil, nil); got != nil {
		t.Fatal("WireTaskWebhook with nil scheduler returned non-nil, want nil")
	}

	// Enabled: URL set → subscribes and delivers.
	t.Setenv("WEBHOOK_CALLBACK_URL", srv.URL)
	unsub := WireTaskWebhook(engine, nil)
	if unsub == nil {
		t.Fatal("WireTaskWebhook with URL set must return a teardown func")
	}
	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"env-wire-task", "", string(obs.EventTaskCompleted), nil))
	if n := drain(deliveries, 1); n != 1 {
		t.Fatalf("expected 1 delivery, got %d", n)
	}
	unsub()
	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"env-wire-task", "", string(obs.EventTaskCompleted), nil))
	if n := drain(deliveries, 0); n > 0 {
		t.Errorf("delivery after unwire: got %d, want 0", n)
	}
}

// TestSubscribeTaskEvents_DoubleWireDocumentsBehaviour pins down the REAL
// behaviour of calling SubscribeTaskEvents twice: it is NOT idempotent. Each
// call appends a handler to BOTH task_completed and task_failed, so wiring
// twice yields 2 live subscribers per event type and every publish delivers
// twice. Production guards against this with a single call site per process
// (see WireTaskWebhook's doc comment), so this test documents the failure mode
// rather than pretending a guarantee the code does not have.
func TestSubscribeTaskEvents_DoubleWireDocumentsBehaviour(t *testing.T) {
	deliveries := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		deliveries <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	engine := newTestEngine(t)
	n := NewWebhookNotifier(srv.URL)
	n.SubscribeTaskEvents(engine)
	n.SubscribeTaskEvents(engine) // deliberately double-wires

	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"double-wire-task", "", string(obs.EventTaskCompleted), nil))
	// 2 subscribers on task_completed -> exactly 2 deliveries for one publish.
	if n2 := drain(deliveries, 2); n2 != 2 {
		t.Errorf("double-wire delivered %d, want 2 (one per live subscription)", n2)
	}
	// A third publish would add 2 more; confirm nothing leaked in the meantime.
	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"double-wire-task", "", string(obs.EventTaskFailed), nil))
	if n3 := drain(deliveries, 2); n3 != 2 {
		t.Errorf("task_failed double-wire delivered %d, want 2", n3)
	}
	// Teardown must remove every subscribed instance.
	n.UnsubscribeTaskEvents()
	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"double-wire-task", "", string(obs.EventTaskCompleted), nil))
	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"double-wire-task", "", string(obs.EventTaskFailed), nil))
	if n4 := drain(deliveries, 0); n4 > 0 {
		t.Errorf("delivery after unwire: got %d, want 0", n4)
	}
}

// TestSubscribeTaskEvents_Guards proves every disabled case subscribes nothing.
func TestSubscribeTaskEvents_Guards(t *testing.T) {
	deliveries := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		deliveries <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	engine := newTestEngine(t)

	// Disabled notifier (empty URL) and nil scheduler must both subscribe nothing.
	if (&webhookNotifier{}).enabled() {
		t.Fatal("empty URL must be disabled")
	}
	(&webhookNotifier{}).SubscribeTaskEvents(engine)
	NewWebhookNotifier(srv.URL).SubscribeTaskEvents(nil)

	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"guard-task", "", string(obs.EventTaskCompleted), nil))
	if n := drain(deliveries, 0); n > 0 {
		t.Errorf("disabled cases delivered %d, want 0", n)
	}
}

// TestSubscribeTaskEvents_RoutesToAgent verifies that WEBHOOK_USE_AGENT=true
// makes the wired subscription POST to the agent route instead of the notify
// route, and that the same event still goes through enrichment + signing.
// This is the end-to-end branch that lets a task terminal event trigger an LLM
// run rather than a bare push notification.
func TestSubscribeTaskEvents_RoutesToAgent(t *testing.T) {
	const secret = "«REDACTED»"
	deliveries := make(chan []byte, 8)
	var paths []string
	var pathMu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathMu.Lock()
		paths = append(paths, r.URL.Path)
		pathMu.Unlock()
		if r.Header.Get("X-Hub-Signature-256") != signedHeaderFor(r, secret) {
			t.Error("HMAC signature rejected")
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		deliveries <- b
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"delivered"}`))
	}))
	defer srv.Close()

	engine := newTestEngine(t)
	t.Setenv("WEBHOOK_USE_AGENT", "true")
	n := NewWebhookNotifier(srv.URL)
	n.secret = secret
	if !n.useAgent {
		t.Fatal("useAgent not parsed from WEBHOOK_USE_AGENT=true")
	}
	n.SubscribeTaskEvents(engine)
	defer n.UnsubscribeTaskEvents()

	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"agent-task", "", string(obs.EventTaskCompleted), map[string]any{"confidence": 0.7}))

	select {
	case b := <-deliveries:
		if !bytes.Contains(b, []byte(`"task_id":"agent-task"`)) {
			t.Errorf("body missing task_id: %s", b)
		}
		if !bytes.Contains(b, []byte(`"status":"completed"`)) {
			t.Errorf("body missing status=completed: %s", b)
		}
	default:
		t.Fatal("no delivery received")
	}

	pathMu.Lock()
	defer pathMu.Unlock()
	if len(paths) != 1 || paths[0] != "/webhooks/callback-agent" {
		t.Errorf("paths = %v, want [/webhooks/callback-agent]", paths)
	}
}

// TestSubscribeTaskEvents_UsesNotifyByDefault pins the default: with no
// WEBHOOK_USE_AGENT set, the wired subscription posts to the notify route, so
// existing deployments are unchanged by the switch.
func TestSubscribeTaskEvents_UsesNotifyByDefault(t *testing.T) {
	deliveries := make(chan struct{}, 8)
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		gotPath = r.URL.Path
		deliveries <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	engine := newTestEngine(t)
	t.Setenv("WEBHOOK_USE_AGENT", "")
	n := NewWebhookNotifier(srv.URL)
	if n.useAgent {
		t.Fatal("useAgent must default to false")
	}
	n.SubscribeTaskEvents(engine)
	defer n.UnsubscribeTaskEvents()

	obs.DefaultBus.Publish(obs.NewAgentEvent(
		"notify-task", "", string(obs.EventTaskCompleted), nil))
	if n2 := drain(deliveries, 1); n2 != 1 {
		t.Fatalf("expected 1 delivery, got %d", n2)
	}
	if gotPath != "/webhooks/callback-notify" {
		t.Errorf("path = %q, want /webhooks/callback-notify", gotPath)
	}
}

// --- helpers ---

// drain removes up to want items from ch and returns how many it got.
func drain(ch <-chan struct{}, want int) int {
	n := 0
	for n < want {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
	return n
}

// signedHeaderFor recomputes the expected HMAC signature for r's body so a test
// can compare it against the header that arrived.
func signedHeaderFor(r *http.Request, secret string) string {
	b, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(b))
	return SignedWebhookHeader(b, secret)
}
