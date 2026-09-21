package core

import (
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"strings"
	"testing"
)

func TestDirectedEngine_ComplexityPreCheck(t *testing.T) {
	reg := &config.Registry{
		Roles:           make(map[string]*config.Role),
		Skills:          make(map[string]*config.Skill),
		MCPs:            make(map[string]*config.MCPDef),
		DefaultProvider: "local",
	}
	reg.Roles["local-role"] = &config.Role{ID: "local-role", Provider: "local", BaseCapability: "test"}
	reg.Roles["remote-role"] = &config.Role{ID: "remote-role", Provider: "anthropic", BaseCapability: "test"}

	logger, _ := NewLogger("test_logs", nil)
	defer logger.Close()

	engine := &DirectedEngine{
		registry: reg,
		logger:   logger,
	}

	tests := []struct {
		name    string
		inputs  []schemas.StepInput
		wantErr bool
		errSub  string
	}{
		{
			name: "Reject single local step",
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "local-role", Task: "Simple Task"},
			},
			wantErr: true,
			errSub:  "too simple",
		},
		{
			name: "Accept single local step with extra skills",
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "local-role", Task: "Task with skill", AdditionalSkills: []string{"skill1"}},
			},
			wantErr: false,
		},
		{
			name: "Accept single remote step",
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "remote-role", Task: "Remote Task"},
			},
			wantErr: false,
		},
		{
			name: "Accept multiple local steps",
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "local-role", Task: "Task 1"},
				{ID: "step2", RoleID: "local-role", Task: "Task 2"},
			},
			wantErr: false,
		},
		{
			name: "Reject single step with default local provider",
			inputs: []schemas.StepInput{
				{ID: "step1", RoleID: "unknown-role", Task: "Default Local Task"},
			},
			wantErr: true,
			errSub:  "too simple",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
