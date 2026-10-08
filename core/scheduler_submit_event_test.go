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
