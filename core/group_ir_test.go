package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
)

func TestDispatchGroup_WithSessionIR(t *testing.T) {
	// 1. Setup registry with a RoleGroup but NO roles
	tmpDir, _ := os.MkdirTemp("", "group_ir_test")
	defer os.RemoveAll(tmpDir)

	configPath := filepath.Join(tmpDir, "config.json")
	reg, _ := config.NewRegistry(configPath)

	groupID := "test_group"
	reg.RoleGroups[groupID] = &config.RoleGroup{
		ID:     groupID,
		Policy: config.GroupPolicySequential,
		Members: []config.GroupMember{
			{RoleID: "session_expert", TaskTemplate: "Do something expert: {{input}}"},
			// Second member to bypass Smart Routing fast-path (single-step
			// no-dep tasks are routed directly to spawner.Spawn without
			// creating a TaskGraph).
			{RoleID: "session_expert", TaskTemplate: "Follow up on: {{input}}"},
		},
	}

	// 2. Create Engine and Dispatcher
	logger, _ := NewLogger(t.TempDir(), nil)
	defer logger.Close()
	engine := NewDirectedEngine(reg, nil, nil, nil, logger, nil, t.TempDir(), t.TempDir(), nil)
	engine.UseSwarm = true // avoid background run

	dispatcher := NewGroupDispatcher(engine, reg)

	// 3. Define Session Role
	sessionRole := &config.Role{
		ID:             "session_expert",
		Name:           "Session Expert",
		BaseCapability: "expert_stuff",
	}

	// 4. Dispatch with Session IR
	taskID, err := dispatcher.DispatchGroup(groupID, "Expert Task", nil, []*config.Role{sessionRole}, nil, nil, "")
	if err != nil {
		t.Fatalf("DispatchGroup failed: %v", err)
	}

	// 5. Verify the generated graph has the session role
	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	engine.Mu.RUnlock()

	if graph == nil {
		for i := 0; i < 20; i++ {
			time.Sleep(50 * time.Millisecond)
			engine.Mu.RLock()
			graph = engine.graphs[taskID]
			engine.Mu.RUnlock()
			if graph != nil {
				break
			}
		}
	}

	if graph == nil {
		t.Fatalf("graph not found in engine")
	}

	hub := NewContextHub(reg, graph, nil)
	resolved := hub.GetRole("session_expert")
	if resolved == nil {
		t.Errorf("failed to resolve session role for group member")
	} else if resolved.Name != "Session Expert" {
		t.Errorf("resolved role name mismatch: %s", resolved.Name)
	}

	// Verify global registry remains clean
	if _, ok := reg.Roles["session_expert"]; ok {
		t.Errorf("session role leaked into global registry from group dispatch")
	}
}
