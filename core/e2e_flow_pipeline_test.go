package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/core/stream"
	"github.com/daybeam/vortex/core/verify"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ============================================================================
// E2E End-to-End Execution Flow Pipeline Test
// Tests the full lifecycle of a task step chaining:
// 1. Task Graph Planning -> 2. Context Assembly -> 3. Stream Protection ->
// 4. Sieve Interception -> 5. Dual-Track Exit Criteria -> 6. AntiPattern Recovery ->
// 7. Dynamic DAG Surgery -> 8. Auto-Deposition & Terminal Protection
// ============================================================================
func TestE2E_FullTaskExecutionFlowPipeline(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	// ------------------------------------------------------------------------
	// Stage 1: Task Planning & Graph Construction
	// ------------------------------------------------------------------------
	t.Log("Stage 1: Planning TaskGraph with steps")
	taskID := "task_e2e_flow_9000"
	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"step_1_build": {
				ID:     "step_1_build",
				Task:   "Build service binary",
				Status: schemas.StepRunning,
			},
			"step_2_test": {
				ID:        "step_2_test",
				Task:      "Run integration test",
				Status:    schemas.StepPending,
				DependsOn: []string{"step_1_build"},
			},
		},
	}

	if graph.IsTerminal() {
		t.Fatalf("Stage 1 Failed: TaskGraph should NOT be terminal initially")
	}

	// ------------------------------------------------------------------------
	// Stage 2: Context Assembly & FMC Experience Injection
	// ------------------------------------------------------------------------
	t.Log("Stage 2: Assembling Active Context with Experience Injection")
	expMockStore := &mockExpStore{
		nodes: []*store.ExperienceNode{
			{
				NodeID:     "node_build_precedent",
				Capability: "software_engineer",
				Strategy:   "Build service binary using make",
				Outcome:    "success",
				Confidence: 0.95,
			},
		},
	}
	assembler := NewActiveContextAssembler(expMockStore, nil, 2048)

	promptContext := assembler.Assemble(ctx, "Build service binary", "step_1_build", "", "software_engineer", "software_engineer")
	if !strings.Contains(promptContext, "[LEARNED EXPERIENCE PRECEDENT]") {
		t.Errorf("Stage 2 Failed: Active Context missing [LEARNED EXPERIENCE PRECEDENT] marker")
	}

	// ------------------------------------------------------------------------
	// Stage 3: Streaming Token Generation & Reasoning Isolation
	// ------------------------------------------------------------------------
	t.Log("Stage 3: Streaming LLM chunks with StreamChunkDetector & Reasoning Isolation")
	detector := stream.NewStreamChunkDetector()

	type StreamChunk struct {
		Content   string
		Reasoning string
	}

	chunks := []StreamChunk{
		{Reasoning: "Analyzing build target and dependencies...\n"},
		{Content: "Executing build command...\n"},
		{Content: "Build completed successfully.\n"},
	}

	var publicText strings.Builder
	var reasoningText strings.Builder

	for i, chunk := range chunks {
		if chunk.Reasoning != "" {
			reasoningText.WriteString(chunk.Reasoning)
		}
		if chunk.Content != "" {
			if err := detector.Check(chunk.Content); err != nil {
				t.Fatalf("Stage 3 Failed on chunk %d: %v", i, err)
			}
			publicText.WriteString(chunk.Content)
		}
	}

	if strings.Contains(publicText.String(), "Analyzing build target") {
		t.Errorf("Stage 3 Failed: Reasoning content leaked into public response text!")
	}

	// ------------------------------------------------------------------------
	// Stage 4: Tool Execution & Sieve Loop Interception
	// ------------------------------------------------------------------------
	t.Log("Stage 4: Sieve Tool Inspection & Poller Loop Interception")
	sieve := NewSieve(100)
	sieve.ToolRepetitionThreshold = 2

	toolCall := store.ToolInteraction{
		ToolName: "check_build_status",
		Arguments: map[string]any{
			"target": "binary_x",
		},
		Result: "building",
	}

	// First call -> OK
	ok, _ := sieve.InspectToolCalls(taskID, "step_1_build", []store.ToolInteraction{toolCall})
	if !ok {
		t.Fatalf("Stage 4 Failed: First tool call should pass Sieve")
	}

	// Repeat call 2 & 3 -> Exceeds threshold of 2
	sieve.InspectToolCalls(taskID, "step_1_build", []store.ToolInteraction{toolCall})
	ok, reason := sieve.InspectToolCalls(taskID, "step_1_build", []store.ToolInteraction{toolCall})
	if ok {
		t.Fatalf("Stage 4 Failed: Expected Sieve to intercept tool repetition on 3rd call")
	}
	if reason != "InfiniteLoopDetected" {
		t.Errorf("Stage 4 Failed: Expected reason 'InfiniteLoopDetected', got: %s", reason)
	}

	// ------------------------------------------------------------------------
	// Stage 5: Dual-Track Exit Criteria Verification (Zero Token Short Circuit)
	// ------------------------------------------------------------------------
	t.Log("Stage 5: Dual-Track Verification Short Circuit on Missing File")
	outputFile := filepath.Join(tempDir, "service_bin")
	verifier := &verify.FileExistsVerifier{}

	criteria := map[string]any{
		"path": outputFile,
	}

	startTime := time.Now()
	passed, _, err := verifier.Verify(ctx, tempDir, criteria)
	duration := time.Since(startTime)

	if err != nil || passed {
		t.Fatalf("Stage 5 Failed: FileExistsVerifier should fail for missing file without error")
	}
	if duration > 10*time.Millisecond {
		t.Errorf("Stage 5 Failed: Verifier duration %v exceeded 10ms threshold for zero-token check", duration)
	}

	// ------------------------------------------------------------------------
	// Stage 6: AntiPattern Guidance Injection on Retry
	// ------------------------------------------------------------------------
	t.Log("Stage 6: AntiPattern Guidance Injection on Retry")
	antiPatternBlock := fmt.Sprintf("%s\nANTI-PATTERN: Do not check build status without timeout\nEND %s\n",
		MarkerPrevFailureAntiPattern, MarkerPrevFailureAntiPattern)

	recoveryPrompt := composeRecoveryPrompt("Build service binary", "file not found: service_bin", antiPatternBlock)

	if !strings.Contains(recoveryPrompt, "file not found: service_bin") {
		t.Errorf("Stage 6 Failed: Recovery prompt missing rejection reason")
	}
	if !strings.Contains(recoveryPrompt, "Do not check build status without timeout") {
		t.Errorf("Stage 6 Failed: Recovery prompt missing AntiPattern guidance")
	}

	// ------------------------------------------------------------------------
	// Stage 7: Dynamic DAG Surgery on Dead-End Step
	// ------------------------------------------------------------------------
	t.Log("Stage 7: Dynamic DAG Surgery on Unrecoverable Dead-End")
	// Mark step_1_build as deprecated
	graph.Steps["step_1_build"].Status = schemas.StepDeprecated

	// Surgery: Inject step_1_fallback and update step_2_test dependencies
	fallbackStep := &schemas.Step{
		ID:     "step_1_fallback",
		Task:   "Build binary using precompiled container",
		Status: schemas.StepRunning,
	}
	graph.Steps[fallbackStep.ID] = fallbackStep

	// Redirect step_2_test dependency
	graph.Steps["step_2_test"].DependsOn = []string{"step_1_fallback"}

	if graph.Steps["step_1_build"].Status != schemas.StepDeprecated {
		t.Errorf("Stage 7 Failed: Expected step_1_build status to be StepDeprecated")
	}
	if graph.Steps["step_2_test"].DependsOn[0] != "step_1_fallback" {
		t.Errorf("Stage 7 Failed: Expected step_2_test dependency to be step_1_fallback")
	}

	// Complete fallback step and downstream step
	graph.Steps["step_1_fallback"].Status = schemas.StepOK
	graph.Steps["step_2_test"].Status = schemas.StepOK
	graph.Status = schemas.GraphCompleted

	// ------------------------------------------------------------------------
	// Stage 8: Terminal State Protection & Auto-Deposition
	// ------------------------------------------------------------------------
	t.Log("Stage 8: Terminal State Protection & Auto-Deposition")
	if !graph.IsTerminal() {
		t.Fatalf("Stage 8 Failed: Expected TaskGraph to report IsTerminal() == true")
	}

	t.Log("E2E Task Execution Flow Pipeline completed successfully!")
}
