package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Regression test for docs/TASK_WEBHOOK_CALLBACK_DESIGN.md §3:
// Single-step fast-path tasks must publish terminal EventBus events.
//
// Bug: The goBackground closure in scheduler_submit.go set graph.Status
// and closed doneChans but never called publishEvent. Webhook/callback
// subscribers (DefaultBus) never heard about single-step task completion
// or failure — the most common task type. The fix adds publishEvent calls
// after broadcastDone, mirroring what finalize() does for multi-step tasks.
//
// Link: docs/TASK_WEBHOOK_CALLBACK_DESIGN.md §3 Option 1
// Fix: core/scheduler_submit.go — publishEvent after broadcastDone in goBackground.

func TestSubmit_SingleStepPublishesTerminalEvent(t *testing.T) {
	providers.ClearCache()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Mock server that returns a simple completion response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		respJSON, _ := json.Marshal("OK")
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {\"content\": %s}}]}\n\n", respJSON)
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {}, \"finish_reason\": \"stop\"}]}\n\n")
		fmt.Fprintln(w, "data: [DONE]")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	cfg := config.Config{
		DefaultProvider: "test_p",
		Providers: map[string]config.ProviderConfig{
			"test_p": {
				Provider: "openai",
				Model:    "gpt-4",
				BaseURL:  srv.URL,
				APIKey:   "fake",
			},
		},
		Roles: []config.Role{
			{ID: "worker", BaseCapability: "text"},
		},
	}
	data, _ := json.Marshal(cfg)
	os.WriteFile(configPath, data, 0644)

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ts := store.NewTaskStore(store.NewFileTaskBackend(filepath.Join(tmpDir, "tasks")))
	es := mustNewExperienceStore(t, filepath.Join(tmpDir, "exp"), ts, nil, nil, nil)
	logger := mustNewLogger(t, filepath.Join(tmpDir, "logs"), &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() })

	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, tmpDir, tmpDir, nil)
	defer engine.Stop()

	// Subscribe to EventTaskCompleted on DefaultBus.
	completedCh := make(chan string, 4)
	unsub := DefaultBus.SubscribeWithCancel(string(EventTaskCompleted), func(e AgentEvent) error {
		select {
		case completedCh <- e.TaskID:
		default:
		}
		return nil
	})
	defer unsub()

	// Submit a single-step task (the fast-path that §3 identified as broken).
	inputs := []schemas.StepInput{
		{ID: "step1", RoleID: "worker", Task: "Reply with the single word OK.", ProviderOverride: "test_p"},
	}
	taskID, err := engine.Submit(inputs)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait for the terminal event. Before the fix, this times out because
	// the goBackground closure never calls publishEvent.
	select {
	case receivedID := <-completedCh:
		if receivedID != taskID {
			t.Errorf("expected task_id %s, got %s", taskID, receivedID)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("EventTaskCompleted not published within 15s — " +
			"single-step fast-path task did not publish terminal event (§3 regression)")
	}
}

// TestSubmit_SingleStepPartialReachesTerminal is the C5DD-T1 regression test
// (PREREG-C5DD §7.4 / 铁律 42: 先跑出红再改).
//
// Bug: a single-step (smart_route) task whose step returned a self-declared
// StatusPartial (not a ParseOutput fallback) raised a step_failed decision
// and returned from the goBackground closure. But single-step tasks never
// start the run() loop (startGraphExecutor is multi-step only), so the
// decision's Broadcast() had no consumer — the step sat in StepPartial and
// task_status stayed "running" until the harness budget timer expired. The
// task never reached a terminal state on its own.
//
// Fix (honest terminal, PREREG-C5DD §7.2): partial ≠ success, so the fast
// path now sets graph.Status = GraphFailed (preserving step.Status =
// StepPartial + ResultRef) and falls through to the terminal-publish block,
// emitting EventTaskFailed. No decision is raised on a loop-less path.
//
// Red before fix: EventTaskFailed never arrives → 20s timeout.
// Green after fix: EventTaskFailed published, graph "failed", step "partial".
func TestSubmit_SingleStepPartialReachesTerminal(t *testing.T) {
	providers.ClearCache()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Mock server returns a self-declared partial result (valid JSON with
	// status=partial). ParseOutput returns this directly (no "not parseable"
	// fallback assumption), so isParseFallback=false and the fast path hits
	// the StatusPartial branch — not the parse-fallback completion escape.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		partialJSON := `{"status": "partial", "confidence": 0.9, "result": {"answer": "incomplete"}}`
		respJSON, _ := json.Marshal(partialJSON)
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {\"content\": %s}}]}\n\n", respJSON)
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {}, \"finish_reason\": \"stop\"}]}\n\n")
		fmt.Fprintln(w, "data: [DONE]")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	cfg := config.Config{
		DefaultProvider: "test_p",
		Providers: map[string]config.ProviderConfig{
			"test_p": {
				Provider: "openai",
				Model:    "gpt-4",
				BaseURL:  srv.URL,
				APIKey:   "fake",
			},
		},
		Roles: []config.Role{
			{ID: "worker", BaseCapability: "text"},
		},
	}
	data, _ := json.Marshal(cfg)
	os.WriteFile(configPath, data, 0644)

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ts := store.NewTaskStore(store.NewFileTaskBackend(filepath.Join(tmpDir, "tasks")))
	es := mustNewExperienceStore(t, filepath.Join(tmpDir, "exp"), ts, nil, nil, nil)
	logger := mustNewLogger(t, filepath.Join(tmpDir, "logs"), &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() })

	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, tmpDir, tmpDir, nil)
	defer engine.Stop()

	// Subscribe to EventTaskFailed — the honest terminal signal for partial.
	failedCh := make(chan string, 4)
	unsub := DefaultBus.SubscribeWithCancel(string(EventTaskFailed), func(e AgentEvent) error {
		select {
		case failedCh <- e.TaskID:
		default:
		}
		return nil
	})
	defer unsub()

	inputs := []schemas.StepInput{
		{ID: "step1", RoleID: "worker", Task: "Return a partial result.", ProviderOverride: "test_p"},
	}
	taskID, err := engine.Submit(inputs)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	select {
	case receivedID := <-failedCh:
		if receivedID != taskID {
			t.Errorf("expected task_id %s, got %s", taskID, receivedID)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("EventTaskFailed not published within 20s — " +
			"single-step partial task did not reach terminal " +
			"(C5DD-T1 regression: task stuck in running, PREREG-C5DD §7.2)")
	}

	// Honesty checks: graph failed (partial ≠ success), step preserved as
	// partial (not collapsed to OK).
	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	graphStatus := schemas.GraphRunning
	stepStatus := schemas.StepPending
	if graph != nil {
		graphStatus = graph.Status
		if st := graph.Steps["step1"]; st != nil {
			stepStatus = st.Status
		}
	}
	engine.Mu.RUnlock()

	if graphStatus != schemas.GraphFailed {
		t.Errorf("expected GraphFailed for partial single-step, got %v", graphStatus)
	}
	if stepStatus != schemas.StepPartial {
		t.Errorf("expected StepPartial preserved (honesty), got %v", stepStatus)
	}
}

// TestSubmitDecision_SmartRoutedSkipReachesTerminal is the C5DD-T2 regression
// test (PREREG-C5DD §7.2 / 铁律 42: 先跑出红再改).
//
// Bug: a single-step (smart_route) task whose step returned
// StatusDelegationRequired raised a DecisionDelegationRequired and returned
// from the goBackground closure. When the decision was resolved with "skip"
// via SubmitDecision, the function recreated the doneChan and called
// Broadcast() — but no run() loop exists for smart-routed tasks, so the
// graph stayed GraphRunning and the doneChan was never closed.
//
// Fix: SubmitDecisionWithPayload's IsSmartRouted branch finalizes inline
// (mirroring FulfillStep): determines terminal status from step states
// (all-skipped → GraphCompletedWithSkips), closes doneChan, persists,
// and publishes — all without relying on a run() consumer.
//
// Red before fix: doneChan never closes → 10s timeout.
// Green after fix: doneChan closed, graph "completed_with_skips".
func TestSubmitDecision_SmartRoutedSkipReachesTerminal(t *testing.T) {
	providers.ClearCache()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Mock server returns OK so the single-step task completes normally.
	// We then reset the graph to the delegation-blocked state manually
	// (what the fastpath's delegationBlocked branch would produce).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		okJSON := `{"status": "ok", "confidence": 1.0, "result": {"answer": "done"}}`
		respJSON, _ := json.Marshal(okJSON)
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {\"content\": %s}}]}\n\n", respJSON)
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {}, \"finish_reason\": \"stop\"}]}\n\n")
		fmt.Fprintln(w, "data: [DONE]")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	cfg := config.Config{
		DefaultProvider: "test_p",
		Providers: map[string]config.ProviderConfig{
			"test_p": {
				Provider: "openai",
				Model:    "gpt-4",
				BaseURL:  srv.URL,
				APIKey:   "fake",
			},
		},
		Roles: []config.Role{
			{ID: "worker", BaseCapability: "text"},
		},
	}
	data, _ := json.Marshal(cfg)
	os.WriteFile(configPath, data, 0644)

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ts := store.NewTaskStore(store.NewFileTaskBackend(filepath.Join(tmpDir, "tasks")))
	es := mustNewExperienceStore(t, filepath.Join(tmpDir, "exp"), ts, nil, nil, nil)
	logger := mustNewLogger(t, filepath.Join(tmpDir, "logs"), &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() })

	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, tmpDir, tmpDir, nil)
	defer engine.Stop()

	// Subscribe to EventTaskCompleted to know when the initial task completes.
	completedCh := make(chan string, 4)
	unsubCompleted := DefaultBus.SubscribeWithCancel(string(EventTaskCompleted), func(e AgentEvent) error {
		select {
		case completedCh <- e.TaskID:
		default:
		}
		return nil
	})
	defer unsubCompleted()

	inputs := []schemas.StepInput{
		{ID: "step1", RoleID: "worker", Task: "Return ok.", ProviderOverride: "test_p"},
	}
	taskID, err := engine.Submit(inputs)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait for initial completion.
	select {
	case <-completedCh:
	case <-time.After(20 * time.Second):
		t.Fatal("initial task did not complete within 20s")
	}

	// Reset graph to the delegation-blocked state (what the fastpath's
	// delegationBlocked branch would produce: step=StepBlocked, graph=
	// GraphRunning, one pending DecisionDelegationRequired).
	decisionID := "test-delegation-decision"
	engine.Mu.Lock()
	graph := engine.graphs[taskID]
	if graph == nil {
		engine.Mu.Unlock()
		t.Fatal("graph not found after completion")
	}
	graph.Status = schemas.GraphRunning
	step := graph.Steps["step1"]
	if step == nil {
		engine.Mu.Unlock()
		t.Fatal("step1 not found")
	}
	step.Status = schemas.StepBlocked
	graph.PendingDecisions = []*schemas.Decision{{
		ID:      decisionID,
		StepID:  "step1",
		Type:    schemas.DecisionDelegationRequired,
		Options: []string{"fulfill", "skip", "abort"},
		Created: time.Now(),
	}}
	// Recreate doneChan so we can detect closure by the fix.
	engine.doneChans[taskID] = make(chan struct{})
	doneCh := engine.doneChans[taskID]
	engine.Mu.Unlock()

	// Resolve the decision with "skip" — the auto-resolve default
	// (defaultNonInteractiveChoice picks "skip" first for delegation).
	err = engine.SubmitDecision(taskID, decisionID, "skip")
	if err != nil {
		t.Fatalf("SubmitDecision failed: %v", err)
	}

	// Assert: doneChan closes (terminal reached).
	// Before fix: doneChan is recreated unclosed by the existing code → timeout.
	// After fix: smart-routed branch closes doneChan → immediate receive.
	select {
	case <-doneCh:
		// doneChan closed — terminal reached
	case <-time.After(10 * time.Second):
		t.Fatal("doneChan not closed within 10s — " +
			"smart-routed skip did not reach terminal " +
			"(C5DD-T2 regression: graph stuck in running, PREREG-C5DD §7.2)")
	}

	// Honesty checks: graph reached completed_with_skips, step is skipped.
	engine.Mu.RLock()
	graphStatus := schemas.GraphRunning
	stepStatus := schemas.StepPending
	if g := engine.graphs[taskID]; g != nil {
		graphStatus = g.Status
		if st := g.Steps["step1"]; st != nil {
			stepStatus = st.Status
		}
	}
	engine.Mu.RUnlock()

	if graphStatus != schemas.GraphCompletedWithSkips {
		t.Errorf("expected GraphCompletedWithSkips, got %v", graphStatus)
	}
	if stepStatus != schemas.StepSkipped {
		t.Errorf("expected StepSkipped, got %v", stepStatus)
	}
}
