package core

import (
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// ── buildPreviousSummary tests ─────────────────────────────────────────

func TestBuildPreviousSummary_NoArchive_ReturnsDefault(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)
	// engine.Archive is nil

	dep := &schemas.Step{ID: "s1", ResultRef: "", TargetFile: ""}
	got := engine.buildPreviousSummary(dep, "task1")
	if !strings.Contains(got, "no previous output") {
		t.Fatalf("expected default '(no previous output...)' message, got: %s", got)
	}
}

func TestBuildPreviousSummary_TargetFile_Fallback(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)
	// No archive, but TargetFile is set

	dep := &schemas.Step{ID: "s1", TargetFile: "/tmp/artifact.json"}
	got := engine.buildPreviousSummary(dep, "task1")
	if !strings.Contains(got, "/tmp/artifact.json") {
		t.Fatalf("expected TargetFile fallback mentioning the path, got: %s", got)
	}
	if !strings.Contains(got, "artifact exists") {
		t.Fatalf("expected 'artifact exists' in fallback, got: %s", got)
	}
}

func TestBuildPreviousSummary_ResultRef_ArchiveHit(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	archivePath := t.TempDir() + "/archive.jsonl"
	engine.Archive = NewContextArchive(archivePath)

	// Append a memory item that will be found by SearchAllStepSummaries.
	err := engine.Archive.Append(MemoryItem{
		TaskID:    "task1",
		NodeID:    "node-1",
		Summary:   "Revenue analysis complete: Q3=$2.4M, Q4=$3.1M",
		StepIDs:   []string{"s1"},
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("failed to append archive item: %v", err)
	}

	dep := &schemas.Step{ID: "s1", ResultRef: "Revenue analysis"}
	got := engine.buildPreviousSummary(dep, "task1")
	if !strings.Contains(got, "Revenue analysis") {
		t.Fatalf("expected ResultRef-based summary, got: %s", got)
	}
}

func TestBuildPreviousSummary_StepID_ArchiveHit(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	archivePath := t.TempDir() + "/archive.jsonl"
	engine.Archive = NewContextArchive(archivePath)

	// Append an item with step ID "s1" in StepIDs.
	err := engine.Archive.Append(MemoryItem{
		TaskID:    "task1",
		NodeID:    "node-2",
		Summary:   "File converted: output.csv with 1500 rows",
		StepIDs:   []string{"s1"},
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("failed to append archive item: %v", err)
	}

	// ResultRef is empty, so it should fall through to Source 2 (step ID match).
	dep := &schemas.Step{ID: "s1", ResultRef: ""}
	got := engine.buildPreviousSummary(dep, "task1")
	if !strings.Contains(got, "output.csv") {
		t.Fatalf("expected step-ID-based summary, got: %s", got)
	}
}

// TestBuildPreviousSummary_NoStepMatch_DoesNotReturnForeignSummary is the
// regression test for audit L-17-1 (AUDIT_REPORT_2026-10-17.md §L-17-1).
//
// Before the fix, buildPreviousSummary had a "best-effort fallback" that
// returned summaries[0] — the most recent summary from ANY step — when the
// requested step's own summary was absent. This injected a foreign step's
// output as the upstream step's "previous output", misleading the model.
//
// After the fix, when the step's own summary is not found, the function falls
// through to the TargetFile hint or the "(no previous output)" default.
//
// This test fails before the fix (returns s3's summary) and passes after.
func TestBuildPreviousSummary_NoStepMatch_DoesNotReturnForeignSummary(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	archivePath := t.TempDir() + "/archive.jsonl"
	engine.Archive = NewContextArchive(archivePath)

	// Archive contains a summary for step "s3" only — NOT for the requested "s1".
	foreignSummary := "Step s3 fetched user profiles: 42 records"
	if err := engine.Archive.Append(MemoryItem{
		TaskID:    "task1",
		NodeID:    "node-foreign",
		Summary:   foreignSummary,
		StepIDs:   []string{"s3"},
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("failed to append archive item: %v", err)
	}

	// Request previous output for step "s1" (no ResultRef, no TargetFile).
	dep := &schemas.Step{ID: "s1", ResultRef: "", TargetFile: ""}
	got := engine.buildPreviousSummary(dep, "task1")

	// Must NOT return the foreign step's summary.
	if strings.Contains(got, foreignSummary) {
		t.Fatalf("audit L-17-1 regression: buildPreviousSummary returned a foreign step's "+
			"summary as s1's previous output.\n  got: %s\n  foreign summary must not leak: %s",
			got, foreignSummary)
	}
	// Must return the "(no previous output)" default.
	if !strings.Contains(got, "no previous output") {
		t.Fatalf("expected default '(no previous output...)' message when step's own "+
			"summary is absent, got: %s", got)
	}
}

// ── tryBacktrack delta mode tests ──────────────────────────────────────

func TestTryBacktrack_DeltaMode_SetsPreviousOutputSummary(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	archivePath := t.TempDir() + "/archive.jsonl"
	engine.Archive = NewContextArchive(archivePath)

	// Pre-populate archive with upstream's previous output.
	_ = engine.Archive.Append(MemoryItem{
		TaskID:    "t1",
		NodeID:    "n1",
		Summary:   "Previous: data fetched from API, 200 records",
		StepIDs:   []string{"s1"},
		Timestamp: time.Now(),
	})

	upstream := &schemas.Step{ID: "s1", Status: schemas.StepOK, ResultRef: "data fetched"}
	downstream := &schemas.Step{
		ID:            "s2",
		DependsOn:     []string{"s1"},
		MaxLoopRounds: 3,
		Status:        schemas.StepPartial,
	}
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": upstream, "s2": downstream},
	}
	output := &schemas.SubagentOutput{MissingContext: []string{"missing revenue column"}}

	handled := engine.tryBacktrack(graph, downstream, output)
	if !handled {
		t.Fatal("expected tryBacktrack to return true")
	}

	if upstream.PreviousOutputSummary == "" {
		t.Fatal("expected upstream PreviousOutputSummary to be set")
	}
	if !strings.Contains(upstream.PreviousOutputSummary, "Previous:") {
		t.Fatalf("expected PreviousOutputSummary to contain archive content, got: %s", upstream.PreviousOutputSummary)
	}
}

func TestTryBacktrack_DeltaMode_SetsDeltaBacktrackFlag(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	upstream := &schemas.Step{ID: "s1", Status: schemas.StepOK, TargetFile: "/tmp/out.json"}
	downstream := &schemas.Step{
		ID:            "s2",
		DependsOn:     []string{"s1"},
		MaxLoopRounds: 2,
		Status:        schemas.StepPartial,
	}
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": upstream, "s2": downstream},
	}
	output := &schemas.SubagentOutput{MissingContext: []string{"x"}}

	engine.tryBacktrack(graph, downstream, output)

	if upstream.Metadata == nil {
		t.Fatal("expected upstream Metadata to be initialized")
	}
	if !upstream.Metadata.DeltaBacktrack {
		t.Fatal("expected upstream Metadata.DeltaBacktrack == true")
	}
}

func TestTryBacktrack_DeltaMode_FeedbackContainsPreviousSummary(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	upstream := &schemas.Step{
		ID:         "s1",
		Status:     schemas.StepOK,
		TargetFile: "/tmp/report.md",
	}
	downstream := &schemas.Step{
		ID:            "s2",
		DependsOn:     []string{"s1"},
		MaxLoopRounds: 3,
		Status:        schemas.StepPartial,
	}
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": upstream, "s2": downstream},
	}
	output := &schemas.SubagentOutput{MissingContext: []string{"缺少格式化字段"}}

	engine.tryBacktrack(graph, downstream, output)

	// The AdditionalPromptContext should contain the MarkerSystemFeedback.
	if !strings.Contains(upstream.AdditionalPromptContext, MarkerSystemFeedback) {
		t.Fatal("expected feedback to contain MarkerSystemFeedback")
	}
	// It should mention the upstream step ID.
	if !strings.Contains(upstream.AdditionalPromptContext, "s1") {
		t.Fatal("expected feedback to mention upstream step s1")
	}
	// It should mention the downstream step ID.
	if !strings.Contains(upstream.AdditionalPromptContext, "s2") {
		t.Fatal("expected feedback to mention downstream step s2")
	}
	// It should contain the supplement instruction.
	if !strings.Contains(upstream.AdditionalPromptContext, "补充") {
		t.Fatal("expected feedback to contain supplement instruction")
	}
	// It should contain the round counter.
	if !strings.Contains(upstream.AdditionalPromptContext, "1/3") {
		t.Fatal("expected feedback to contain round counter 1/3")
	}
}

func TestTryBacktrack_DeltaMode_NoPrevious_StillWorks(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)
	// No archive, no TargetFile, no ResultRef — first execution.

	upstream := &schemas.Step{ID: "s1", Status: schemas.StepOK}
	downstream := &schemas.Step{
		ID:            "s2",
		DependsOn:     []string{"s1"},
		MaxLoopRounds: 2,
		Status:        schemas.StepPartial,
	}
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": upstream, "s2": downstream},
	}
	output := &schemas.SubagentOutput{MissingContext: []string{"data"}}

	handled := engine.tryBacktrack(graph, downstream, output)
	if !handled {
		t.Fatal("expected tryBacktrack to return true even without previous output")
	}
	// PreviousOutputSummary should still be set (to the default message).
	if upstream.PreviousOutputSummary == "" {
		t.Fatal("expected PreviousOutputSummary to be set even with no previous output")
	}
	if !strings.Contains(upstream.PreviousOutputSummary, "no previous output") {
		t.Fatalf("expected default message, got: %s", upstream.PreviousOutputSummary)
	}
	// DeltaBacktrack flag should still be set.
	if upstream.Metadata == nil || !upstream.Metadata.DeltaBacktrack {
		t.Fatal("expected DeltaBacktrack flag to be set")
	}
}

func TestTryBacktrack_DeltaMode_LogsEventDeltaBacktrack(t *testing.T) {
	engine := newTestEngineForCycleBreaker(t)

	upstream := &schemas.Step{ID: "s1", Status: schemas.StepOK, TargetFile: "/tmp/x"}
	downstream := &schemas.Step{
		ID:            "s2",
		DependsOn:     []string{"s1"},
		MaxLoopRounds: 3,
		Status:        schemas.StepPartial,
	}
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": upstream, "s2": downstream},
	}
	output := &schemas.SubagentOutput{MissingContext: []string{"x"}}

	engine.tryBacktrack(graph, downstream, output)

	// The cycle counter should be incremented.
	cycleID := makeCycleID("s2", []string{"s1"})
	if graph.CycleCounters[cycleID] != 1 {
		t.Fatalf("expected CycleCounters[%s] == 1, got %d", cycleID, graph.CycleCounters[cycleID])
	}
}

// ── Retry cleanup test ─────────────────────────────────────────────────

func TestRetryClearsDeltaBacktrackState(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	// Set up a graph with a step in delta-backtrack mode and a pending decision.
	upstream := &schemas.Step{
		ID:                     "s1",
		Status:                 schemas.StepPending,
		PreviousOutputSummary:  "stale previous output that should be cleared",
		AdditionalPromptContext: "stale cycle-breaker feedback",
		Metadata:               &schemas.Metadata{DeltaBacktrack: true},
	}
	downstream := &schemas.Step{
		ID:                     "s2",
		DependsOn:              []string{"s1"},
		Status:                 schemas.StepFailed,
		PreviousOutputSummary:  "stale downstream previous",
		AdditionalPromptContext: "stale downstream feedback",
		Metadata:               &schemas.Metadata{DeltaBacktrack: true},
	}
	graph := &schemas.TaskGraph{
		TaskID: "task-retry-delta",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": upstream, "s2": downstream},
		PendingDecisions: []*schemas.Decision{{
			ID:      "dec-1",
			StepID:  "s2",
			Type:    schemas.DecisionStepFailed,
			Options: []string{"skip", "abort", "retry"},
			Created: time.Now(),
		}},
	}

	s.Mu.Lock()
	s.graphs["task-retry-delta"] = graph
	s.Mu.Unlock()

	// User chooses "retry".
	if err := s.SubmitDecision("task-retry-delta", "dec-1", "retry"); err != nil {
		t.Fatalf("SubmitDecision failed: %v", err)
	}

	// Verify delta state is cleared on the downstream step (s2).
	if downstream.PreviousOutputSummary != "" {
		t.Fatalf("expected downstream PreviousOutputSummary cleared, got: %s", downstream.PreviousOutputSummary)
	}
	if downstream.AdditionalPromptContext != "" {
		t.Fatalf("expected downstream AdditionalPromptContext cleared, got: %s", downstream.AdditionalPromptContext)
	}
	if downstream.Metadata != nil && downstream.Metadata.DeltaBacktrack {
		t.Fatal("expected downstream Metadata.DeltaBacktrack == false")
	}

	// Verify delta state is cleared on the upstream dependency (s1).
	if upstream.PreviousOutputSummary != "" {
		t.Fatalf("expected upstream PreviousOutputSummary cleared, got: %s", upstream.PreviousOutputSummary)
	}
	if upstream.AdditionalPromptContext != "" {
		t.Fatalf("expected upstream AdditionalPromptContext cleared, got: %s", upstream.AdditionalPromptContext)
	}
	if upstream.Metadata != nil && upstream.Metadata.DeltaBacktrack {
		t.Fatal("expected upstream Metadata.DeltaBacktrack == false")
	}

	// Verify the step is reset to Pending for re-execution.
	if downstream.Status != schemas.StepPending {
		t.Fatalf("expected downstream Status == StepPending, got %v", downstream.Status)
	}
}

func TestRefineAndRetryClearsPreviousOutputSummary(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	// Step was in delta-backtrack mode; user chooses "refine_and_retry"
	// with their own feedback.
	step := &schemas.Step{
		ID:                     "s1",
		Status:                 schemas.StepFailed,
		PreviousOutputSummary:  "stale previous output",
		AdditionalPromptContext: "stale cycle-breaker feedback",
		Metadata:               &schemas.Metadata{DeltaBacktrack: true},
	}
	graph := &schemas.TaskGraph{
		TaskID: "task-refine-delta",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": step},
		PendingDecisions: []*schemas.Decision{{
			ID:      "dec-1",
			StepID:  "s1",
			Type:    schemas.DecisionStepFailed,
			Context: map[string]any{"feedback": "user says: try using the v2 API endpoint"},
			Options: []string{"skip", "abort", "refine_and_retry"},
			Created: time.Now(),
		}},
	}

	s.Mu.Lock()
	s.graphs["task-refine-delta"] = graph
	s.Mu.Unlock()

	if err := s.SubmitDecision("task-refine-delta", "dec-1", "refine_and_retry"); err != nil {
		t.Fatalf("SubmitDecision failed: %v", err)
	}

	// PreviousOutputSummary should be cleared.
	if step.PreviousOutputSummary != "" {
		t.Fatalf("expected PreviousOutputSummary cleared, got: %s", step.PreviousOutputSummary)
	}
	// DeltaBacktrack flag should be cleared.
	if step.Metadata != nil && step.Metadata.DeltaBacktrack {
		t.Fatal("expected Metadata.DeltaBacktrack == false")
	}
	// AdditionalPromptContext should be replaced with user feedback.
	if step.AdditionalPromptContext != "user says: try using the v2 API endpoint" {
		t.Fatalf("expected AdditionalPromptContext to be user feedback, got: %s", step.AdditionalPromptContext)
	}
}
