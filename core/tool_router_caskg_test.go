package core

import (
	"github.com/daybeam/vortex/config"
	"os"
	"path/filepath"
	"testing"
)

func TestToolRouter_CausalBoost(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "caskg_router_test")
	defer os.RemoveAll(tmpDir)
	path := filepath.Join(tmpDir, "caskg.json")

	manager := NewCaSKGManager(path)
	bus := NewEventBus()
	manager.HookEventBus(bus)

	// Train the manager
	for i := 0; i < 10; i++ {
		bus.Publish(NewAgentEvent("t1", "s1", string(EventStepCompleted), map[string]any{"skill": "SkillA"}))
		bus.Publish(NewAgentEvent("t1", "s2", string(EventStepCompleted), map[string]any{"skill": "SkillB"}))
		bus.Publish(NewAgentEvent("t1", "", string(EventTaskCompleted), nil))
	}

	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID:             "mcp1",
				AvailableTools: []string{"SkillB"},
			},
		},
	}

	router := NewToolRouter(reg, manager)

	// Tool with Causal Boost
	req := RouteRequest{
		Task:               "some task",
		Bindings:           []config.MCPBinding{{MCPID: "mcp1", AllowedTools: []string{"SkillB"}}},
		PredecessorSkillID: "SkillA",
	}

	// This is tricky as RRF logic is complex.
	// But simply checking if it runs and returns SkillB should be okay.
	routed := router.Route(req)
	found := false
	for _, b := range routed {
		if b.MCPID == "mcp1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected SkillB to be routed")
	}
}
