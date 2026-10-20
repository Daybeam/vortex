package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// The system prompt used to advertise write_file / read_file / execute_code
// unconditionally. That was true for every role until appworld_agent declared
// core_tools: ["read_file","write_file"], at which point the prompt promised a
// code sandbox the dispatcher would refuse. The model took the promise at face
// value: on 2026-10-19 (AppWorld 4fab96f_3) 7 of its 8 core calls were
// execute_code against a sandbox holding no app data, 0 were MCP calls, and the
// run ended in MAX_TURNS_EXHAUSTED.
//
// The fix is that the advertisement is now derived from the same policy the
// dispatcher enforces. These tests pin the three states of that policy.
func TestBuild_CoreToolSectionFollowsRoleCoreTools(t *testing.T) {
	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)

	t.Run("nil means unrestricted", func(t *testing.T) {
		got := collectPrompt(t, pa, &config.Role{ID: "executor", BaseCapability: "do things"})
		for _, want := range []string{"**write_file**", "**read_file**", "**execute_code**", "Use execute_code for computation"} {
			if !strings.Contains(got, want) {
				t.Errorf("unrestricted role lost %q; existing configs would silently shrink", want)
			}
		}
	})

	t.Run("file tools only omits the code sandbox", func(t *testing.T) {
		got := collectPrompt(t, pa, &config.Role{
			ID:             "appworld_agent",
			BaseCapability: "do things",
			CoreTools:      []string{"read_file", "write_file"},
		})
		if strings.Contains(got, "**execute_code**") {
			t.Error("prompt advertises execute_code for a role whose core_tools exclude it")
		}
		if strings.Contains(got, "Use execute_code for computation") {
			t.Error("prompt still tells the model to reach for execute_code")
		}
		if !strings.Contains(got, "**write_file**") || !strings.Contains(got, "**read_file**") {
			t.Error("prompt dropped file tools the role does have")
		}
		// The task-shared output directory is still the hand-off channel, so its
		// explanation must survive the narrowing.
		if !strings.Contains(got, "SHARED by every step of this task") {
			t.Error("shared output directory explanation was lost along with the code sandbox")
		}
	})

	t.Run("empty list says so instead of staying silent", func(t *testing.T) {
		got := collectPrompt(t, pa, &config.Role{
			ID:             "task_planner",
			BaseCapability: "plan",
			CoreTools:      []string{},
		})
		if strings.Contains(got, "## Core File & Code Tools") {
			t.Error("prompt kept the core-tool heading for a role restricted to nothing")
		}
		for _, banned := range []string{"**write_file**", "**read_file**", "**execute_code**"} {
			if strings.Contains(got, banned) {
				t.Errorf("prompt advertises %s for a role restricted to nothing", banned)
			}
		}
		if !strings.Contains(got, "no built-in file or code tools") {
			t.Error("prompt does not state the absence, so the model will look for the tools anyway")
		}
	})

	t.Run("unknowable names do not widen the surface", func(t *testing.T) {
		got := collectPrompt(t, pa, &config.Role{
			ID:             "typo_role",
			BaseCapability: "do things",
			CoreTools:      []string{"read_files", "execute_code"},
		})
		if strings.Contains(got, "**read_file**") {
			t.Error("a typo in core_tools widened the advertised surface")
		}
		if !strings.Contains(got, "**execute_code**") {
			t.Error("the valid entry in core_tools stopped being advertised")
		}
	})
}
