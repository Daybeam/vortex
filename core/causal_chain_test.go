package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/env"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func TestSpawner_CausalChainCapture(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Mock server for OpenAI protocol
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// First turn: Tool call
		// Second turn: Final answer
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		messages := body["messages"].([]any)
		lastMsg := messages[len(messages)-1].(map[string]any)
		lastContent, _ := lastMsg["content"].(string)

		if !strings.Contains(lastContent, "[ENVIRONMENT OBSERVATION]") {
			// Turn 0
			resp := map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]any{
							"content": "<thought>I need to read the file.</thought>",
							"tool_calls": []map[string]any{
								{
									"id":   "tc1",
									"type": "function",
									"function": map[string]any{
										"name":      "read_file",
										"arguments": `{"path": "test.txt", "_reason": "checking file"}`,
									},
								},
							},
						},
						"finish_reason": "tool_calls",
					},
				},
				"usage": map[string]any{"total_tokens": 10},
			}
			json.NewEncoder(w).Encode(resp)
		} else {
			// Turn 1
			finalResp := map[string]any{
				"status":     "ok",
				"confidence": 1.0,
				"result":     map[string]any{"content": "file read successfully"},
				"capability": "text",
			}
			respJSON, _ := json.Marshal(finalResp)
			content := fmt.Sprintf("<thought>The file is read. Finalizing.</thought>\n```json\n%s\n```", string(respJSON))

			resp := map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]any{
							"content": content,
						},
						"finish_reason": "stop",
					},
				},
				"usage": map[string]any{"total_tokens": 10},
			}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer srv.Close()

	cfg := config.Config{
		DefaultProvider: "p1",
		Providers: map[string]config.ProviderConfig{
			"p1": {
				Provider: "openai",
				Model:    "gpt-4",
				BaseURL:  srv.URL,
				APIKey:   "fake",
			},
		},
		MCPs: []config.MCPDef{
			{
				ID:             "local",
				Command:        env.GetPythonCmd(),
				Args:           []string{"--version"},
				AvailableTools: []string{"read_file"},
			},
		},
		Roles: []config.Role{
			{
				ID:               "worker",
				BaseCapability:   "text",
				BoundMCPBindings: []config.MCPBinding{{MCPID: "local"}},
			},
		},
	}
	data, _ := json.Marshal(cfg)
	os.WriteFile(configPath, data, 0644)

	reg, _ := config.NewRegistry(configPath)
	ts := store.NewTaskStore(store.NewFileTaskBackend(filepath.Join(tmpDir, "tasks")))
	logger, _ := NewLogger(filepath.Join(tmpDir, "logs"), &config.SystemSettings{})
	defer logger.Close()
	spawner := NewSpawner(reg, ts, nil, logger, nil, "outputs")

	graph := &schemas.TaskGraph{
		TaskID:          "task1",
		DecisionHistory: make(map[string]*schemas.DecisionNode),
	}
	hub := NewContextHub(reg, graph, nil)

	req := &SpawnRequest{
		TaskID: "task1",
		StepID: "step1",
		RoleID: "worker",
		Task:   "Read test.txt",
		Hub:    hub,
	}

	res, err := spawner.Spawn(context.Background(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

	if res.Output.Status != "ok" {
		t.Errorf("expected status ok, got %s", res.Output.Status)
	}

	// Verify Decision History
	if len(graph.DecisionHistory) != 2 {
		t.Errorf("expected 2 decisions, got %d", len(graph.DecisionHistory))
	}

	d0 := graph.DecisionHistory["dec_step1_0"]
	if d0 == nil {
		t.Fatal("decision 0 missing")
	}
	if d0.Reasoning != "I need to read the file." {
		t.Errorf("unexpected reasoning 0: %q", d0.Reasoning)
	}
	if d0.Action != "call_tools: [read_file]" {
		t.Errorf("unexpected action 0: %q", d0.Action)
	}

	d1 := graph.DecisionHistory["dec_step1_1"]
	if d1 == nil {
		t.Fatal("decision 1 missing")
	}
	if d1.Reasoning != "The file is read. Finalizing." {
		t.Errorf("unexpected reasoning 1: %q", d1.Reasoning)
	}
	if d1.Outcome != "ok" {
		t.Errorf("unexpected outcome 1: %q", d1.Outcome)
	}

	// Verify StepResult in store
	sr, _ := ts.Get(context.Background(), "task1", "step1")
	if sr == nil {
		t.Fatal("StepResult missing from store")
	}
	if len(sr.DecisionIDs) != 2 {
		t.Errorf("expected 2 decision IDs in StepResult, got %d", len(sr.DecisionIDs))
	}
}
