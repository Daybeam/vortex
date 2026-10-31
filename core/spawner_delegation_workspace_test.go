package core

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// spawner_delegation_workspace_test.go — D4 regression tests
// (DELEGATION_HANDOVER_REDESIGN.md §4.4, workspace contract in envelope).
//
// Bug: the delegation prompt says "This output directory is SHARED by every
// step of this task" but never injects the directory path. In delegation mode
// the host implements write_file/read_file itself and needs to know WHERE.
// Fix: (1) tryDelegationMode adds a "workspace" block to the envelope with
// task_output_dir / shared_context_dir absolute paths; (2) prompt_assembler
// appends the absolute path to the SHARED-dir text in delegation mode.

// TestDelegationEnvelope_IncludesWorkspacePaths verifies that the delegation
// decision's prompt envelope includes a "workspace" block with the absolute
// task_output_dir and shared_context_dir paths.
func TestDelegationEnvelope_IncludesWorkspacePaths(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Do work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	dec := waitForDelegationDecision(t, engine, taskID)

	prompt, _ := dec.Context["prompt"].(map[string]any)
	if prompt == nil {
		t.Fatalf("expected prompt envelope in decision context, got nil")
	}

	ws, _ := prompt["workspace"].(map[string]any)
	if ws == nil {
		t.Fatalf("expected workspace block in prompt envelope, got nil")
	}

	taskOutputDir, _ := ws["task_output_dir"].(string)
	if taskOutputDir == "" {
		t.Fatalf("expected non-empty task_output_dir in workspace block")
	}

	// Verify the path is absolute (joins outputBase with taskID).
	expectedDir := filepath.Join(engine.outputBase, taskID)
	if taskOutputDir != expectedDir {
		t.Fatalf("expected task_output_dir %q, got %q", expectedDir, taskOutputDir)
	}

	sharedCtxDir, _ := ws["shared_context_dir"].(string)
	if sharedCtxDir == "" {
		t.Fatalf("expected non-empty shared_context_dir in workspace block")
	}
	expectedShared := filepath.Join(engine.outputBase, taskID, "shared_context")
	if sharedCtxDir != expectedShared {
		t.Fatalf("expected shared_context_dir %q, got %q", expectedShared, sharedCtxDir)
	}

	pathSem, _ := ws["path_semantics"].(string)
	if pathSem != "relative to task_output_dir" {
		t.Fatalf("expected path_semantics 'relative to task_output_dir', got %q", pathSem)
	}
}

// TestDelegationPrompt_IncludesAbsolutePath verifies that the system prompt
// in the delegation envelope contains the absolute path of the shared output
// directory, so the host knows where to write/read files.
func TestDelegationPrompt_IncludesAbsolutePath(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "Do work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	dec := waitForDelegationDecision(t, engine, taskID)

	prompt, _ := dec.Context["prompt"].(map[string]any)
	if prompt == nil {
		t.Fatalf("expected prompt envelope in decision context, got nil")
	}

	systemPrompt, _ := prompt["system_prompt"].(string)
	if systemPrompt == "" {
		t.Fatalf("expected non-empty system_prompt in prompt envelope")
	}

	// The D4 fix appends: The absolute path is "<dir>"; write deliverables there...
	// Use %q to match the quoted form in the prompt (backslashes are escaped on Windows).
	expectedDir := fmt.Sprintf("%q", filepath.Join(engine.outputBase, taskID))
	if !strings.Contains(systemPrompt, expectedDir) {
		t.Fatalf("expected system_prompt to contain absolute path %s, but it was not found.\nPrompt (last 500 chars): ...%s",
			expectedDir, systemPrompt[max(0, len(systemPrompt)-500):])
	}
	if !strings.Contains(systemPrompt, "write deliverables there") {
		t.Fatalf("expected system_prompt to contain 'write deliverables there' hint")
	}
}
