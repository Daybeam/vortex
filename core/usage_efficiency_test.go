package core

import (
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"strings"
	"testing"
)

// TestDirectedEngine_ComplexityPreCheck verifies the validateTaskComplexity
// guard behaviour in BOTH the legacy hard-reject mode (RejectSimpleTasks=true)
// and the new default pass-through mode (RejectSimpleTasks=false). The second
// subtest documents that the Smart Routing Fast-Path can now receive
// single-step local tasks that were previously rejected before reaching it.
func TestDirectedEngine_ComplexityPreCheck(t *testing.T) {
	tests := []struct {
		name         string
		rejectSimple bool
		inputs       []schemas.StepInput
		wantErr      bool
		errSub       string
	}{
		// --- RejectSimpleTasks=true (legacy hard-reject behaviour) ---
		{
			name:         "Reject single local step (opt-in)",
			rejectSimple: true,
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "local-role", Task: "Simple Task"},
			},
			wantErr: true,
			errSub:  "too simple",
		},
		{
			name:         "Accept single local step with extra skills (opt-in)",
			rejectSimple: true,
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "local-role", Task: "Task with skill", AdditionalSkills: []string{"skill1"}},
			},
			wantErr: false,
		},
		{
			name:         "Accept single remote step (opt-in)",
			rejectSimple: true,
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "remote-role", Task: "Remote Task"},
			},
			wantErr: false,
		},
		{
			name:         "Accept multiple local steps (opt-in)",
			rejectSimple: true,
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "local-role", Task: "Task 1"},
				{ID: "step2", RoleID: "local-role", Task: "Task 2"},
			},
			wantErr: false,
		},
		{
			name:         "Reject single step with default local provider (opt-in)",
			rejectSimple: true,
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "unknown-role", Task: "Default Local Task"},
			},
			wantErr: true,
			errSub:  "too simple",
		},

		// --- RejectSimpleTasks=false (default; FastPath can reach these) ---
		{
			name:         "Accept single local step (default off)",
			rejectSimple: false,
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "local-role", Task: "Simple Task"},
			},
			wantErr: false,
		},
		{
			name:         "Accept single step with default local provider (default off)",
			rejectSimple: false,
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "unknown-role", Task: "Default Local Task"},
			},
			wantErr: false,
		},
		{
			name:         "Reject empty inputs (independent of flag)",
			rejectSimple: false,
			inputs:       []schemas.StepInput{},
			wantErr:      true,
			errSub:       "steps is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &config.Registry{
				Roles:           make(map[string]*config.Role),
				Skills:          make(map[string]*config.Skill),
				MCPs:            make(map[string]*config.MCPDef),
				DefaultProvider: "local",
			}
			reg.Roles["local-role"] = &config.Role{ID: "local-role", Provider: "local", BaseCapability: "test"}
			reg.Roles["remote-role"] = &config.Role{ID: "remote-role", Provider: "anthropic", BaseCapability: "test"}
			reg.System.RejectSimpleTasks = tt.rejectSimple

			logger := mustNewLogger(t, t.TempDir(), nil) // audit T-PATH-2: use t.TempDir() instead of hardcoded "test_logs"
			defer logger.Close()

			engine := &DirectedEngine{
				registry: reg,
				logger:   logger,
			}

			err := engine.validateTaskComplexity(tt.inputs)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateTaskComplexity() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && tt.errSub != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.errSub)) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.errSub)
			}
		})
	}
}

// TestDirectedEngine_RejectSimpleTasks_DefaultsOff confirms the guard is
// opt-in: an unconfigured deployment must NOT reject single-step local tasks,
// so they can reach the Smart Routing Fast-Path (scheduler_submit.go:249).
// Regression guard for the 2026-10-20 change that moved the guard behind
// System.RejectSimpleTasks.
func TestDirectedEngine_RejectSimpleTasks_DefaultsOff(t *testing.T) {
	reg := &config.Registry{
		Roles:           make(map[string]*config.Role),
		Skills:          make(map[string]*config.Skill),
		MCPs:            make(map[string]*config.MCPDef),
		DefaultProvider: "local",
	}
	engine := &DirectedEngine{registry: reg}

	if engine.rejectSimpleTasksEnabled() {
		t.Fatal("RejectSimpleTasks must default to false")
	}

	// With the flag off, a single local step with no skills/MCPs must pass.
	err := engine.validateTaskComplexity([]schemas.StepInput{
		{ID: "step1", RoleID: "unknown-role", Task: "Simple Task"},
	})
	if err != nil {
		t.Fatalf("expected no error with RejectSimpleTasks=false, got: %v", err)
	}
}

// TestDirectedEngine_RejectSimpleTasks_NilRegistryConfirm nil-safe: the
// helper must return false (guard off) when registry is nil, so a partially
// initialised engine never rejects tasks.
func TestDirectedEngine_RejectSimpleTasks_NilRegistry(t *testing.T) {
	engine := &DirectedEngine{} // registry == nil
	if engine.rejectSimpleTasksEnabled() {
		t.Fatal("rejectSimpleTasksEnabled() must be false when registry is nil")
	}
}
