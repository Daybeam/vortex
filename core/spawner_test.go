package core

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestSpawner_PruneSkills removed (fixes audit T-C20): pruneSkills moved to
// SkillRouter. The skip-only stub provided zero coverage. If pruneSkills
// behavior needs testing, add tests in skill_router_test.go.

func TestSpawner_FailFast_MissingCapabilities(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"strict_role": {
				ID:          "strict_role",
				Requires:    []string{"pdf.extract", "fragment.search"},
				Generatable: false,
			},
			"generatable_role": {
				ID:          "generatable_role",
				Requires:    []string{"pdf.extract"},
				Generatable: true,
			},
		},
		Skills: map[string]*config.Skill{},
		MCPs:   map[string]*config.MCPDef{},
	}
	logDir, err := os.MkdirTemp("", "spawner-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	logger := mustNewLogger(t, logDir, &config.SystemSettings{})
	s := &Spawner{
		registry: reg,
		logger:   logger,
	}

	ctx := context.Background()
	hub := NewContextHub(reg, nil, nil)

	// Test hard failure
	req1 := &SpawnRequest{RoleID: "strict_role", TaskID: "t1", StepID: "s1", Hub: hub}
	_, err = s.doSpawn(ctx, req1)
	if err == nil || !strings.Contains(err.Error(), "Blocked") {
		t.Errorf("expected Blocked error for strict_role, got %v", err)
	}

	// Test dynamic escalation failure
	req2 := &SpawnRequest{RoleID: "generatable_role", TaskID: "t1", StepID: "s2", Hub: hub}
	_, err = s.doSpawn(ctx, req2)
	if err == nil || !strings.Contains(err.Error(), "DYNAMIC_ESCALATION_REQUIRED") {
		t.Errorf("expected DYNAMIC_ESCALATION_REQUIRED error for generatable_role, got %v", err)
	}
}
