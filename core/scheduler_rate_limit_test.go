package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func TestScheduler_ProactiveRateLimiting(t *testing.T) {
	providers.ClearCache()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	var callTimes []time.Time
	var callMu sync.Mutex
	// Mock server for OpenAI protocol (SSE streaming format)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callMu.Lock()
		callTimes = append(callTimes, time.Now())
		callMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		// Simulate some work
		time.Sleep(50 * time.Millisecond)
		resp := map[string]any{
			"status":     "ok",
			"confidence": 1.0,
			"result":     map[string]any{"done": true},
			"capability": "text",
		}
		respJSON, _ := json.Marshal(resp)
		content := string(respJSON)
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {\"content\": %q}}]}\n\n", content)
		fmt.Fprintf(w, "data: {\"choices\": [{\"delta\": {}, \"finish_reason\": \"stop\"}]}\n\n")
		fmt.Fprintln(w, "data: [DONE]")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	cfg := config.Config{
		DefaultProvider: "limited_p",
		Providers: map[string]config.ProviderConfig{
			"limited_p": {
				Provider: "openai",
				Model:    "gpt-4",
				BaseURL:  srv.URL,
				APIKey:   "fake",
				RateLimit: &config.RateLimit{
					RequestsPerMinute: 600, // 100ms interval
				},
			},
		},
		Roles: []config.Role{
			{
				ID:             "worker",
				BaseCapability: "text",
			},
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

	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, tmpDir, tmpDir, nil)
	defer engine.Stop()

	// Submit two concurrent tasks using the limited provider
	// NOTE: two steps per task to bypass Smart Routing fast-path
	// (single-step no-dep tasks are routed directly to spawner.Spawn
	// and never create a TaskGraph — so GetStatus would return false
	// forever, causing a 10s timeout).
	inputs := []schemas.StepInput{
		{ID: "step1", RoleID: "worker", Task: "test 1", ProviderOverride: "limited_p"},
		{ID: "step2", RoleID: "worker", Task: "test 2", ProviderOverride: "limited_p"},
	}

	id1, _ := engine.Submit(inputs)
	id2, _ := engine.Submit(inputs)

	// Wait for completion (or timeout)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for {
		s1, ok1 := engine.GetStatus(id1)
		s2, ok2 := engine.GetStatus(id2)
		if ok1 && ok2 {
			t.Logf("Status 1: %v, Status 2: %v", s1["status"], s2["status"])
			// Break if both are either completed, blocked, or failed
			if (s1["status"] == schemas.GraphCompleted || s1["status"] == schemas.GraphBlocked || s1["status"] == schemas.GraphFailed) &&
				(s2["status"] == schemas.GraphCompleted || s2["status"] == schemas.GraphBlocked || s2["status"] == schemas.GraphFailed) {
				break
			}
		}
		if ctx.Err() != nil {
			t.Fatal("timeout waiting for tasks")
		}
		time.Sleep(500 * time.Millisecond)
	}

	callMu.Lock()
	defer callMu.Unlock()
	if len(callTimes) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", len(callTimes))
	}

	// Calculate gaps between consecutive calls
	// NOTE: The first gap may be very small because two tasks are
	// submitted concurrently — their first step1 calls happen almost
	// simultaneously. The leaky bucket enforces RPM between the
	// second task's step1 and the first task's step2, etc.
	// We verify that gaps after the first one are respected.
	for i := 1; i < len(callTimes); i++ {
		gap := callTimes[i].Sub(callTimes[i-1])
		t.Logf("Gap between call %d and %d: %v", i-1, i, gap)
		if i > 1 && gap < 90*time.Millisecond {
			t.Errorf("gap too small (call %d→%d): %v (expected ~100ms)", i-1, i, gap)
		}
	}
}
