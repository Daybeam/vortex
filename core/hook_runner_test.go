package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestHookRunner_HooksForPoint verifies that hooks are filtered by point.
func TestHookRunner_HooksForPoint(t *testing.T) {
	hooks := []config.HookConfig{
		{Point: HookChatHabit, Script: "a.lua"},
		{Point: HookChatPreTurn, Script: "b.lua"},
		{Point: HookChatPreTurn, Script: "c.lua"},
		{Point: HookStepPost, Script: "d.lua"},
	}
	r := NewHookRunner(hooks, config.ExternalRuntimes{}, nil)

	if got := len(r.HooksForPoint(HookChatHabit)); got != 1 {
		t.Errorf("expected 1 chat_habit hook, got %d", got)
	}
	if got := len(r.HooksForPoint(HookChatPreTurn)); got != 2 {
		t.Errorf("expected 2 chat_pre_turn hooks, got %d", got)
	}
	if got := len(r.HooksForPoint(HookStepPost)); got != 1 {
		t.Errorf("expected 1 step_post hook, got %d", got)
	}
	if got := len(r.HooksForPoint(HookTaskCompletion)); got != 0 {
		t.Errorf("expected 0 task_completion hooks, got %d", got)
	}
}

// TestHookRunner_FilterMatching verifies that filters correctly match or reject.
func TestHookRunner_FilterMatching(t *testing.T) {
	tests := []struct {
		name   string
		filter config.HookFilter
		ctx    HookContext
		want   bool
	}{
		{
			name:   "no filter matches everything",
			filter: config.HookFilter{},
			ctx:    HookContext{RoleID: "engineer"},
			want:   true,
		},
		{
			name:   "role_id exact match",
			filter: config.HookFilter{RoleID: "engineer"},
			ctx:    HookContext{RoleID: "engineer"},
			want:   true,
		},
		{
			name:   "role_id mismatch",
			filter: config.HookFilter{RoleID: "engineer"},
			ctx:    HookContext{RoleID: "auditor"},
			want:   false,
		},
		{
			name:   "task_contains match (case-insensitive)",
			filter: config.HookFilter{TaskContains: "go test"},
			ctx:    HookContext{Input: map[string]any{"task": "Run GO TEST suite"}},
			want:   true,
		},
		{
			name:   "task_contains no match",
			filter: config.HookFilter{TaskContains: "python"},
			ctx:    HookContext{Input: map[string]any{"task": "write go code"}},
			want:   false,
		},
		{
			name:   "task_contains matches user_message when task absent",
			filter: config.HookFilter{TaskContains: "hello"},
			ctx:    HookContext{Input: map[string]any{"user_message": "Hello world"}},
			want:   true,
		},
		{
			name:   "step_id_glob match",
			filter: config.HookFilter{StepIDGlob: "s*"},
			ctx:    HookContext{StepID: "s1"},
			want:   true,
		},
		{
			name:   "step_id_glob no match",
			filter: config.HookFilter{StepIDGlob: "s*"},
			ctx:    HookContext{StepID: "t1"},
			want:   false,
		},
		{
			name:   "task_type match",
			filter: config.HookFilter{TaskType: "coding"},
			ctx:    HookContext{Input: map[string]any{"task_type": "coding"}},
			want:   true,
		},
		{
			name:   "task_type mismatch",
			filter: config.HookFilter{TaskType: "coding"},
			ctx:    HookContext{Input: map[string]any{"task_type": "research"}},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchFilter(tt.filter, tt.ctx)
			if got != tt.want {
				t.Errorf("matchFilter() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHookRunner_LuaExecution verifies that a Lua hook script runs and returns
// the expected result.
func TestHookRunner_LuaExecution(t *testing.T) {
	// Create a temporary Lua script that sets hook_result.action
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "test_hook.lua")
	scriptContent := `
hook_result = {
  action = "inject_context",
  data = {
    context = "You have asked about testing 3 times."
  }
}
`
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0644); err != nil {
		t.Fatal(err)
	}

	logger, _ := NewLogger(filepath.Join(dir, "logs"), nil)
	defer logger.Close()

	hooks := []config.HookConfig{
		{Point: HookChatHabit, Script: scriptPath, TimeoutMs: 5000},
	}
	r := NewHookRunner(hooks, config.ExternalRuntimes{}, logger)

	result := r.Run(context.Background(), HookChatHabit, HookContext{
		Point:     HookChatHabit,
		SessionID: "test_session",
		Input:     map[string]any{"user_message": "how to test?"},
	})

	if result.Action != "inject_context" {
		t.Errorf("expected action 'inject_context', got %q", result.Action)
	}
	if ctx, _ := result.Data["context"].(string); ctx != "You have asked about testing 3 times." {
		t.Errorf("unexpected context: %v", result.Data["context"])
	}
}

// TestHookRunner_FailSafe_NonexistentScript verifies that a missing script
// is logged and skipped without crashing.
func TestHookRunner_FailSafe_NonexistentScript(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewLogger(filepath.Join(dir, "logs"), nil)
	defer logger.Close()

	hooks := []config.HookConfig{
		{Point: HookChatHabit, Script: filepath.Join(dir, "nonexistent.lua")},
	}
	r := NewHookRunner(hooks, config.ExternalRuntimes{}, logger)

	result := r.Run(context.Background(), HookChatHabit, HookContext{
		Point: HookChatHabit,
	})

	// Should return "none" action (fail-safe)
	if result.Action != "none" {
		t.Errorf("expected action 'none' for failed hook, got %q", result.Action)
	}
}

// TestHookRunner_FilteredHookSkipped verifies that a hook whose filter doesn't
// match is not executed.
func TestHookRunner_FilteredHookSkipped(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "test_hook.lua")
	// This script would set action to "abort" if run
	scriptContent := `hook_result = { action = "abort", data = { reason = "should not run" } }`
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0644); err != nil {
		t.Fatal(err)
	}

	logger, _ := NewLogger(filepath.Join(dir, "logs"), nil)
	defer logger.Close()

	hooks := []config.HookConfig{
		{
			Point:  HookChatPreTurn,
			Script: scriptPath,
			Filter: config.HookFilter{RoleID: "auditor"}, // won't match
		},
	}
	r := NewHookRunner(hooks, config.ExternalRuntimes{}, logger)

	result := r.Run(context.Background(), HookChatPreTurn, HookContext{
		Point:  HookChatPreTurn,
		RoleID: "engineer", // doesn't match filter
	})

	if result.Action != "none" {
		t.Errorf("expected action 'none' for filtered-out hook, got %q", result.Action)
	}
}

// TestHookRunner_EmptyHooks verifies that a runner with no hooks returns "none".
func TestHookRunner_EmptyHooks(t *testing.T) {
	r := NewHookRunner(nil, config.ExternalRuntimes{}, nil)
	result := r.Run(context.Background(), HookChatHabit, HookContext{Point: HookChatHabit})
	if result.Action != "none" {
		t.Errorf("expected 'none' with no hooks, got %q", result.Action)
	}
}
