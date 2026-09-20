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

// TestSubmit_TaskContextDerivedFromLifecycleCtx is the regression test for
// audit C2: task contexts in scheduler_submit.go must be derived from
// s.lifecycleCtx (not context.Background()) so that cancelling lifecycleCtx
// cancels running tasks.
//
// We test this directly by calling lifecycleCancel() (not Stop(), which
// would block on bgWg.Wait()) and verifying the provider HTTP request is
// cancelled. This isolates the C2 fix from the C1 fix and from HTTP client
// timing variability.
//
// If C2 is broken (context.Background() used), cancelling lifecycleCtx has
// no effect on the task's context, and the provider call hangs forever.
func TestSubmit_TaskContextDerivedFromLifecycleCtx(t *testing.T) {
	providers.ClearCache()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Mock server that sends one SSE event, then blocks until the request
	// context is cancelled (client disconnect).
	requestReceived := make(chan struct{})
	requestExited := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestReceived)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {\"content\": \"thinking...\"}}]}\n\n")
		w.(http.Flusher).Flush()

		<-r.Context().Done()
		close(requestExited)
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
	es, _ := store.NewExperienceStore(filepath.Join(tmpDir, "exp"), ts, nil, nil, nil)
	logger, _ := NewLogger(filepath.Join(tmpDir, "logs"), &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() })

	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, tmpDir, tmpDir, nil)
	defer engine.Stop()

	inputs := []schemas.StepInput{
		{ID: "step1", RoleID: "worker", Task: "block until cancelled", ProviderOverride: "test_p"},
	}
	taskID, err := engine.Submit(inputs)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	t.Logf("submitted task %s", taskID)

	// Wait for the provider call to start.
	select {
	case <-requestReceived:
	case <-time.After(15 * time.Second):
		t.Fatal("provider call did not start within 15s")
	}

	// Cancel lifecycleCtx directly. This is what Stop() does first, before
	// bgWg.Wait(). By testing it separately, we isolate the C2 fix.
	engine.lifecycleCancel()

	// C2: The task's context should be cancelled, causing the HTTP request
	// to be cancelled and the server handler to exit.
	select {
	case <-requestExited:
		// C2 is fixed: task context was derived from lifecycleCtx.
	case <-time.After(15 * time.Second):
		t.Fatal("provider call did not exit within 15s of lifecycleCancel() — " +
			"task context is NOT derived from lifecycleCtx (audit C2 regression)")
	}
}
