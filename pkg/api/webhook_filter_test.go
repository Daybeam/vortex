package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
)

// --- matchTriggerOn ---

func TestMatchTriggerOn_Tier0Default(t *testing.T) {
	// Empty TriggerOn → Tier 0: task_completed + task_failed only.
	if !matchTriggerOn(string(obs.EventTaskCompleted), nil) {
		t.Error("task_completed should match Tier 0")
	}
	if !matchTriggerOn(string(obs.EventTaskFailed), nil) {
		t.Error("task_failed should match Tier 0")
	}
	if matchTriggerOn(string(obs.EventStepCompleted), nil) {
		t.Error("step_completed should NOT match Tier 0")
	}
}

func TestMatchTriggerOn_ExplicitList(t *testing.T) {
	triggers := []string{"step_failed", "step_retrying"}
	if !matchTriggerOn("step_failed", triggers) {
		t.Error("step_failed should match explicit list")
	}
	if matchTriggerOn("task_completed", triggers) {
		t.Error("task_completed should NOT match explicit list")
	}
}

func TestMatchTriggerOn_NoMatch(t *testing.T) {
	triggers := []string{"step_failed"}
	if matchTriggerOn("task_completed", triggers) {
		t.Error("task_completed should not match [step_failed]")
	}
}

// --- matchGlob ---

func TestMatchGlob_EmptyOrStar(t *testing.T) {
	if !matchGlob("anything", "") {
		t.Error("empty pattern should match all")
	}
	if !matchGlob("anything", "*") {
		t.Error("* pattern should match all")
	}
}

func TestMatchGlob_PrefixMatch(t *testing.T) {
	if !matchGlob("invest_123", "invest_*") {
		t.Error("invest_* should match invest_123")
	}
	if matchGlob("research_456", "invest_*") {
		t.Error("invest_* should NOT match research_456")
	}
}

func TestMatchGlob_ExactMatch(t *testing.T) {
	if !matchGlob("fetch_data", "fetch_data") {
		t.Error("exact pattern should match exact value")
	}
	if matchGlob("fetch_data_v2", "fetch_data") {
		t.Error("exact pattern should NOT match different value")
	}
}

// --- matchTaskType ---

func TestMatchTaskType_EmptyOrStar(t *testing.T) {
	ev := obs.NewAgentEvent("t1", "", "step_completed", nil)
	if !matchTaskType(ev, "", nil) {
		t.Error("empty taskType should match all")
	}
	if !matchTaskType(ev, "*", nil) {
		t.Error("* taskType should match all")
	}
}

func TestMatchTaskType_PayloadRoleID(t *testing.T) {
	ev := obs.NewAgentEvent("t1", "", "step_completed", map[string]any{
		"role_id": "invest",
	})
	if !matchTaskType(ev, "invest", nil) {
		t.Error("payload role_id=invest should match taskType=invest")
	}
	if matchTaskType(ev, "research", nil) {
		t.Error("payload role_id=invest should NOT match taskType=research")
	}
}

func TestMatchTaskType_LookupFailureAllows(t *testing.T) {
	// When scheduler can't find the task, matchTaskType should allow
	// (conservative: don't silently drop events).
	ev := obs.NewAgentEvent("unknown-task", "", "step_completed", nil)
	engine := newTestEngine(t)
	if !matchTaskType(ev, "invest", engine) {
		t.Error("lookup failure should default to allow")
	}
}

// --- matchRoute ---

func TestMatchRoute_DisabledRoute(t *testing.T) {
	route := config.NotificationRoute{
		Enabled:   false,
		TriggerOn: []string{"task_completed"},
		TargetURL: "http://example.com",
	}
	ev := obs.NewAgentEvent("t1", "", string(obs.EventTaskCompleted), nil)
	if matchRoute(ev, route, nil) {
		t.Error("disabled route should never match")
	}
}

func TestMatchRoute_AllLayersPass(t *testing.T) {
	route := config.NotificationRoute{
		Enabled:    true,
		TriggerOn:  []string{"step_failed"},
		TaskIDGlob: "invest_*",
		StepIDGlob: "fetch_*",
		TaskType:   "invest",
		TargetURL:  "http://example.com",
	}
	ev := obs.NewAgentEvent("invest_123", "fetch_data", "step_failed", map[string]any{
		"role_id": "invest",
	})
	if !matchRoute(ev, route, nil) {
		t.Error("all layers should pass")
	}
}

func TestMatchRoute_TriggerOnFails(t *testing.T) {
	route := config.NotificationRoute{
		Enabled:   true,
		TriggerOn: []string{"step_failed"},
		TargetURL: "http://example.com",
	}
	ev := obs.NewAgentEvent("t1", "", "task_completed", nil)
	if matchRoute(ev, route, nil) {
		t.Error("triggerOn mismatch should fail")
	}
}

func TestMatchRoute_TaskIDGlobFails(t *testing.T) {
	route := config.NotificationRoute{
		Enabled:    true,
		TriggerOn:  []string{"task_completed"},
		TaskIDGlob: "invest_*",
		TargetURL:  "http://example.com",
	}
	ev := obs.NewAgentEvent("research_123", "", string(obs.EventTaskCompleted), nil)
	if matchRoute(ev, route, nil) {
		t.Error("task_id_glob mismatch should fail")
	}
}

func TestMatchRoute_StepIDGlobFails(t *testing.T) {
	route := config.NotificationRoute{
		Enabled:    true,
		TriggerOn:  []string{"step_failed"},
		StepIDGlob: "fetch_*",
		TargetURL:  "http://example.com",
	}
	ev := obs.NewAgentEvent("t1", "parse_data", "step_failed", nil)
	if matchRoute(ev, route, nil) {
		t.Error("step_id_glob mismatch should fail")
	}
}

func TestMatchRoute_Tier0DefaultTriggerOn(t *testing.T) {
	// Empty TriggerOn → Tier 0 (task_completed + task_failed).
	route := config.NotificationRoute{
		Enabled:   true,
		TargetURL: "http://example.com",
	}
	evCompleted := obs.NewAgentEvent("t1", "", string(obs.EventTaskCompleted), nil)
	if !matchRoute(evCompleted, route, nil) {
		t.Error("Tier 0 should match task_completed")
	}
	evFailed := obs.NewAgentEvent("t1", "", string(obs.EventTaskFailed), nil)
	if !matchRoute(evFailed, route, nil) {
		t.Error("Tier 0 should match task_failed")
	}
	evStep := obs.NewAgentEvent("t1", "s1", string(obs.EventStepCompleted), nil)
	if matchRoute(evStep, route, nil) {
		t.Error("Tier 0 should NOT match step_completed")
	}
}

// --- hasEnabledRoutes ---

func TestHasEnabledRoutes_Empty(t *testing.T) {
	if hasEnabledRoutes(nil) {
		t.Error("nil routes should have no enabled routes")
	}
	if hasEnabledRoutes([]config.NotificationRoute{}) {
		t.Error("empty routes should have no enabled routes")
	}
}

func TestHasEnabledRoutes_AllDisabled(t *testing.T) {
	routes := []config.NotificationRoute{
		{Enabled: false, TargetURL: "http://a"},
		{Enabled: false, TargetURL: "http://b"},
	}
	if hasEnabledRoutes(routes) {
		t.Error("all disabled routes should return false")
	}
}

func TestHasEnabledRoutes_SomeEnabled(t *testing.T) {
	routes := []config.NotificationRoute{
		{Enabled: false, TargetURL: "http://a"},
		{Enabled: true, TargetURL: "http://b"},
	}
	if !hasEnabledRoutes(routes) {
		t.Error("at least one enabled route should return true")
	}
}

// --- config-driven subscription integration ---

func TestSubscribeWithRoutes_OnlyMatchingRoutesDeliver(t *testing.T) {
	var investCount, researchCount int32
	investSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		atomic.AddInt32(&investCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer investSrv.Close()
	researchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		atomic.AddInt32(&researchCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer researchSrv.Close()

	engine := newTestEngine(t)

	routes := []config.NotificationRoute{
		{
			Name:       "invest-route",
			Enabled:    true,
			TriggerOn:  []string{"task_completed"},
			TaskIDGlob: "invest_*",
			TargetURL:  investSrv.URL,
		},
		{
			Name:       "research-route",
			Enabled:    true,
			TriggerOn:  []string{"task_completed"},
			TaskIDGlob: "research_*",
			TargetURL:  researchSrv.URL,
		},
	}

	n := NewWebhookNotifier("http://placeholder") // URL set so enabled() returns true
	n.SetRoutes(routes)
	n.SubscribeTaskEvents(engine)
	defer n.UnsubscribeTaskEvents()

	// Publish a task_completed for invest_123 → only investSrv should receive.
	obs.DefaultBus.Publish(obs.NewAgentEvent("invest_123", "", string(obs.EventTaskCompleted), nil))
	drainChan := func(ch chan struct{}) {}
	_ = drainChan
	// Small wait for HTTP delivery (fire-and-forget but synchronous in EventBus).
	waitForCount(&investCount, 1)
	if got := atomic.LoadInt32(&investCount); got != 1 {
		t.Fatalf("invest route: expected 1 delivery, got %d", got)
	}
	if got := atomic.LoadInt32(&researchCount); got != 0 {
		t.Fatalf("research route: expected 0 deliveries, got %d", got)
	}

	// Publish a task_completed for research_456 → only researchSrv should receive.
	obs.DefaultBus.Publish(obs.NewAgentEvent("research_456", "", string(obs.EventTaskCompleted), nil))
	waitForCount(&researchCount, 1)
	if got := atomic.LoadInt32(&researchCount); got != 1 {
		t.Fatalf("research route: expected 1 delivery, got %d", got)
	}
	if got := atomic.LoadInt32(&investCount); got != 1 {
		t.Fatalf("invest route: expected still 1 delivery, got %d", got)
	}
}

func TestSubscribeWithRoutes_DisabledRouteDoesNotDeliver(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	engine := newTestEngine(t)

	routes := []config.NotificationRoute{
		{
			Name:      "disabled-route",
			Enabled:   false,
			TriggerOn: []string{"task_completed"},
			TargetURL: srv.URL,
		},
	}

	n := NewWebhookNotifier("http://placeholder")
	n.SetRoutes(routes)
	// All routes disabled → falls back to hardcoded 8-event path.
	n.SubscribeTaskEvents(engine)
	defer n.UnsubscribeTaskEvents()

	// task_completed is in the hardcoded list, so it WILL deliver via the
	// env-based URL (placeholder), not via the route URL. The route URL
	// server should receive 0.
	obs.DefaultBus.Publish(obs.NewAgentEvent("t1", "", string(obs.EventTaskCompleted), nil))
	// Wait a bit for any potential delivery.
	waitForCount(&count, 0)
	if got := atomic.LoadInt32(&count); got != 0 {
		t.Fatalf("disabled route: expected 0 deliveries, got %d", got)
	}
}

func TestSubscribeWithRoutes_StepIDGlobFiltersStepEvents(t *testing.T) {
	var fetchCount int32
	fetchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		atomic.AddInt32(&fetchCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer fetchSrv.Close()

	engine := newTestEngine(t)

	routes := []config.NotificationRoute{
		{
			Name:       "fetch-failures",
			Enabled:    true,
			TriggerOn:  []string{"step_failed"},
			StepIDGlob: "fetch_*",
			TargetURL:  fetchSrv.URL,
		},
	}

	n := NewWebhookNotifier("http://placeholder")
	n.SetRoutes(routes)
	n.SubscribeTaskEvents(engine)
	defer n.UnsubscribeTaskEvents()

	// step_failed with step_id "fetch_data" → should deliver.
	obs.DefaultBus.Publish(obs.NewAgentEvent("t1", "fetch_data", string(obs.EventStepFailed), nil))
	waitForCount(&fetchCount, 1)
	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Fatalf("fetch route: expected 1 delivery, got %d", got)
	}

	// step_failed with step_id "parse_data" → should NOT deliver.
	obs.DefaultBus.Publish(obs.NewAgentEvent("t1", "parse_data", string(obs.EventStepFailed), nil))
	waitForCount(&fetchCount, 1)
	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Fatalf("fetch route: expected still 1 delivery, got %d", got)
	}
}

// --- backward compatibility ---

func TestSubscribeTaskEvents_NoRoutesBackwardCompat(t *testing.T) {
	// When no routes are configured, the notifier should use the hardcoded
	// 8-event subscription (backward compat). This is already covered by
	// TestWireTaskWebhook_DefaultOff, but we add an explicit guard here.
	deliveries := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		deliveries <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	engine := newTestEngine(t)
	t.Setenv("WEBHOOK_CALLBACK_URL", srv.URL)

	// nil routes → backward compat path.
	unsub := WireTaskWebhook(engine, nil)
	if unsub == nil {
		t.Fatal("WireTaskWebhook with URL set must return non-nil")
	}
	defer unsub()

	// task_completed is in the hardcoded list → should deliver.
	obs.DefaultBus.Publish(obs.NewAgentEvent("compat-task", "", string(obs.EventTaskCompleted), nil))
	if n := drain(deliveries, 1); n != 1 {
		t.Fatalf("backward compat: expected 1 delivery, got %d", n)
	}
}

// waitForCount polls an atomic counter until it reaches want or times out.
func waitForCount(counter *int32, want int32) {
	deadline := make(chan struct{})
	go func() {
		time.Sleep(2 * time.Second)
		close(deadline)
	}()
	for {
		if atomic.LoadInt32(counter) >= want {
			return
		}
		select {
		case <-deadline:
			return
		default:
		}
	}
}
