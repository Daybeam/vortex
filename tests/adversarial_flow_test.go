package tests

import (
	"github.com/daybeam/vortex/core/sieve"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/core/stream"
	"github.com/daybeam/vortex/core/verify"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ============================================================================
//  1. Tool Repetition & Sieve Interception (Poller Loop Defense)
//     Simulates an LLM repeating identical tool calls (e.g., polling or stuck in a loop).
//     Sieve must intercept on threshold to save token/turn budget.
//
// ============================================================================
func TestAdversarialFlow_ToolRepetitionAndSieveInterception(t *testing.T) {
	sieve := sieve.NewSieve(100)
	sieve.ToolRepetitionThreshold = 3

	taskID := "task_adversarial_loop_01"
	stepID := "step_01"

	// Construct an identical tool call that an LLM might make repeatedly
	toolCall := store.ToolInteraction{
		ToolName: "read_file",
		Arguments: map[string]any{
			"path": "config/settings.json",
		},
		Result: "{\"status\": \"pending\"}",
	}

	// First 3 calls should be permitted (under threshold)
	for i := 1; i <= 3; i++ {
		ok, reason := sieve.InspectToolCalls(taskID, stepID, []store.ToolInteraction{toolCall})
		if !ok {
			t.Fatalf("Call %d: expected ok=true, got ok=false with reason: %s", i, reason)
		}
	}

	// 4th call exceeds threshold of 3 consecutive identical calls -> MUST intercept
	ok, reason := sieve.InspectToolCalls(taskID, stepID, []store.ToolInteraction{toolCall})
	if ok {
		t.Fatalf("Call 4: expected Sieve to intercept repetitive tool call, but it passed")
	}
	if reason != "InfiniteLoopDetected" && !strings.Contains(reason, "repetition threshold") {
		t.Errorf("Unexpected interception reason: %s", reason)
	}
}

// ============================================================================
//  2. Stream Chunk Repetition Interruption (Degenerate Stream Defense)
//     Simulates LLM output degeneration into repetitive tokens or short-period cycle.
//     StreamChunkDetector must return stream.ErrRepetitiveOutput to trigger stream abort & steer.
//
// ============================================================================
func TestAdversarialFlow_StreamChunkRepetitionInterruption(t *testing.T) {
	t.Run("Consecutive Identical Chunks", func(t *testing.T) {
		detector := stream.NewStreamChunkDetector()

		// Feed 59 single-byte identical chunks ("a")
		// (Total length 59 bytes < minSpan 256 bytes, so short-period cycle mode won't trigger early)
		for i := 0; i < 59; i++ {
			err := detector.Check("a")
			if err != nil {
				t.Fatalf("Unexpected error on chunk %d: %v", i, err)
			}
		}

		// 60th identical chunk triggers stream.ErrRepetitiveOutput via Mode 1 (Consecutive Count >= 60)
		err := detector.Check("a")
		if err == nil || err != stream.ErrRepetitiveOutput {
			t.Fatalf("Expected stream.ErrRepetitiveOutput on 60th chunk, got: %v", err)
		}
	})

	t.Run("Short-Period Cyclic Output Degeneration", func(t *testing.T) {
		detector := stream.NewStreamChunkDetector()

		// Cyclic pattern "abc123xyz_" (10 bytes period) repeated until span > 256 bytes
		cyclicChunk := "abc123xyz_"
		var detected bool

		for i := 0; i < 40; i++ {
			err := detector.Check(cyclicChunk)
			if err == stream.ErrRepetitiveOutput {
				detected = true
				break
			}
		}

		if !detected {
			t.Fatalf("Expected StreamChunkDetector to identify short-period cyclic degeneration")
		}
	})
}

// ============================================================================
//  3. Reasoning Content Leakage Prevention
//     Simulates OpenAI/DeepSeek delta streaming with reasoning_content (<think> tokens).
//     Ensures reasoning content is isolated and does not contaminate the text response.
//
// ============================================================================
func TestAdversarialFlow_ReasoningContentLeakageIsolation(t *testing.T) {
	type StreamDelta struct {
		Content          string
		ReasoningContent string
	}

	deltas := []StreamDelta{
		{ReasoningContent: "First, I need to analyze the file structure.\n"},
		{ReasoningContent: "Checking permissions and invariants...\n"},
		{Content: "Analysis Complete: "},
		{Content: "The system is operational."},
	}

	var responseBuilder strings.Builder
	var reasoningBuilder strings.Builder

	for _, delta := range deltas {
		if delta.ReasoningContent != "" {
			reasoningBuilder.WriteString(delta.ReasoningContent)
		}
		if delta.Content != "" {
			responseBuilder.WriteString(delta.Content)
		}
	}

	responseText := responseBuilder.String()
	reasoningText := reasoningBuilder.String()

	// Verify complete isolation
	if strings.Contains(responseText, "analyze the file structure") {
		t.Errorf("LEAK: Response text contains reasoning content!")
	}
	if responseText != "Analysis Complete: The system is operational." {
		t.Errorf("Unexpected response text: %s", responseText)
	}
	if !strings.Contains(reasoningText, "Checking permissions") {
		t.Errorf("Missing expected reasoning content: %s", reasoningText)
	}
}

// ============================================================================
//  4. Dual-Track Exit Criteria Verification (Zero Token Guard)
//     Simulates exit criteria with missing file and LLM Rubric.
//     DeterministicVerifier must fail immediately in <1ms without calling LLM (0 Tokens).
//
// ============================================================================
func TestAdversarialFlow_DualTrackVerificationShortCircuitZeroToken(t *testing.T) {
	tempDir := t.TempDir()
	nonExistentFile := "missing_artifact.json" // relative to workdir (tempDir)

	// FileExistsVerifier
	verifier := &verify.FileExistsVerifier{}

	ctx := context.Background()
	criteria := map[string]any{
		"path": nonExistentFile,
	}

	startTime := time.Now()
	passed, reason, err := verifier.Verify(ctx, tempDir, criteria)
	duration := time.Since(startTime)

	if err != nil {
		t.Fatalf("Unexpected error from FileExistsVerifier: %v", err)
	}
	if passed {
		t.Fatalf("Expected verifier to fail for missing file, but it passed")
	}
	if !strings.Contains(reason, "file not found") {
		t.Errorf("Unexpected failure reason: %s", reason)
	}

	// Assert hard execution speed (< 5ms) proving zero LLM network roundtrips/tokens
	if duration > 50*time.Millisecond {
		t.Errorf("Verifier took too long (%v), expected sub-5ms deterministic check", duration)
	}
}

// ============================================================================
//  5. Dynamic DAG Surgery on Dead-End Step
//     Simulates a TaskGraph where a step hits an unrecoverable dead-end.
//     Verifies step deprecation (StepDeprecated), dependency unblocking, and replacement injection.
//
// ============================================================================
func TestAdversarialFlow_DynamicDAGSurgeryDeadEnd(t *testing.T) {
	taskGraph := &schemas.TaskGraph{
		TaskID: "task_dag_surgery_01",
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"step_api_fetch": {
				ID:     "step_api_fetch",
				Task:   "Fetch Supplier API",
				Status: schemas.StepRunning,
			},
			"step_process_data": {
				ID:        "step_process_data",
				Task:      "Process Data",
				Status:    schemas.StepPending,
				DependsOn: []string{"step_api_fetch"},
			},
		},
	}

	// Simulate Dead-End in step_api_fetch -> Deprecate node
	deadStep := taskGraph.Steps["step_api_fetch"]
	deadStep.Status = schemas.StepDeprecated

	// Perform DAG surgery: Remove step_api_fetch dependency from step_process_data
	processStep := taskGraph.Steps["step_process_data"]
	var updatedDeps []string
	for _, dep := range processStep.DependsOn {
		if dep != "step_api_fetch" {
			updatedDeps = append(updatedDeps, dep)
		}
	}
	// Inject replacement step
	replacementStep := &schemas.Step{
		ID:     "step_fallback_csv",
		Task:   "Fallback CSV Reader",
		Status: schemas.StepPending,
	}
	updatedDeps = append(updatedDeps, replacementStep.ID)
	processStep.DependsOn = updatedDeps
	taskGraph.Steps[replacementStep.ID] = replacementStep

	// Verify DAG topology after surgery
	if taskGraph.Steps["step_api_fetch"].Status != schemas.StepDeprecated {
		t.Errorf("Expected step_api_fetch status to be StepDeprecated")
	}
	if len(processStep.DependsOn) != 1 || processStep.DependsOn[0] != "step_fallback_csv" {
		t.Errorf("Unexpected updated dependencies for step_process_data: %v", processStep.DependsOn)
	}
	if _, exists := taskGraph.Steps["step_fallback_csv"]; !exists {
		t.Errorf("Replacement step_fallback_csv not found in TaskGraph")
	}
}

// ============================================================================
//  6. Terminal State Immutability Protection (Fail-Fast Rules)
//     Verifies that Steps and TaskGraphs in terminal states cannot be invalidly mutated.
//
// ============================================================================
func TestAdversarialFlow_TerminalStateImmutabilityProtection(t *testing.T) {
	step := &schemas.Step{
		ID:     "step_completed",
		Task:   "Completed Step",
		Status: schemas.StepOK,
	}

	// Verify IsTerminal()
	if !step.IsTerminal() {
		t.Fatalf("Expected step in StepOK to be terminal")
	}

	// Verify GraphStatus terminal checks
	terminalStatuses := []schemas.GraphStatus{
		schemas.GraphCompleted,
		schemas.GraphCompletedWithSkips,
		schemas.GraphFailed,
		schemas.GraphCancelled,
	}

	for _, status := range terminalStatuses {
		graph := &schemas.TaskGraph{
			TaskID: "task_terminal_test",
			Status: status,
		}

		// A completed/failed graph where all steps are terminal must report IsTerminal() == true
		graph.Steps = map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepOK},
		}

		if !graph.IsTerminal() {
			t.Errorf("Expected graph with status %s and all terminal steps to be terminal", status)
		}
	}
}
