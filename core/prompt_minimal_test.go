package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// collectPrompt flattens every block the assembler emits for one role.
func collectPrompt(t *testing.T, pa *PromptAssembler, role *config.Role) string {
	t.Helper()
	blocks, err := pa.Build(
		context.Background(), nil, role, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-minimal",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(b.Text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// TestBuild_MinimalPromptSuppressesFrameworkFraming pins the two fragments a
// self-contracted role must not receive.
//
// Both were being appended to EVERY role, which made the task_planner's prompt
// self-contradictory: its instruction said "you have no tools", the framework
// said "you have write_file / read_file / execute_code", and config said
// core_tools: []. On 2026-10-19 (gemini-3-flash, AppWorld 4fab96f_3) the model
// resolved that contradiction the expensive way — it spent the response budget
// on a <thought> preamble and then produced half a plan.
func TestBuild_MinimalPromptSuppressesFrameworkFraming(t *testing.T) {
	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	pa := NewPromptAssembler(nil, nil, reg, nil)

	// Baseline: an ordinary executor still gets both fragments (nil
	// core_tools = unrestricted, the default).
	plain := collectPrompt(t, pa, &config.Role{ID: "executor", BaseCapability: "do things"})
	if !strings.Contains(plain, "**write_file**") {
		t.Fatal("baseline prompt lost the core-tool blurb; this test would prove nothing")
	}
	if !strings.Contains(plain, "<thought>") {
		t.Fatal("baseline prompt lost the <thought> allowance; this test would prove nothing")
	}

	// Opted out: neither fragment may appear.
	minimal := collectPrompt(t, pa, &config.Role{
		ID:             "task_planner",
		BaseCapability: "plan",
		MinimalPrompt:  true,
	})
	if strings.Contains(minimal, "**write_file**") {
		t.Error("minimal prompt still claims write_file is available")
	}
	if strings.Contains(minimal, "**execute_code**") {
		t.Error("minimal prompt still claims execute_code is available")
	}
	if strings.Contains(minimal, "<thought>") {
		t.Error("minimal prompt still invites a <thought> preamble, which spends the response budget")
	}
	if strings.Contains(minimal, `"status": "ok" | "partial"`) {
		t.Error("minimal prompt still imposes the SubagentOutput schema on a role that defines its own")
	}

	// It must not become an empty prompt: the generic tool-usage line is still
	// correct for a planner that does have app tools.
	if !strings.Contains(minimal, "## Tool Usage") {
		t.Error("minimal prompt dropped the tool-usage guidance as well; that was not the intent")
	}
}
