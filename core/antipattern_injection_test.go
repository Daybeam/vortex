package core

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// antipattern_injection_test.go — Tests for Module 2 of the Two-Tier
// Clarification & Readiness Design. Verifies that:
//  1. buildAntiPatternGuidance queries the store and formats the block.
//  2. composeRecoveryPrompt layers rejection + anti-pattern + prior context.
//  3. The ChatHarness Readiness Gate (Module 1) appears in the system prompt
//     and the delegate tool description.

// ── buildAntiPatternGuidance ───────────────────────────────────────────────

func TestBuildAntiPatternGuidance_NilExpStore_ReturnsEmpty(t *testing.T) {
	engine := &DirectedEngine{}
	got := engine.buildAntiPatternGuidance("some task", "some reason")
	if got != "" {
		t.Fatalf("expected empty guidance when expStore is nil, got %q", got)
	}
}

func TestBuildAntiPatternGuidance_EmptyIntent_ReturnsEmpty(t *testing.T) {
	apStore := store.NewAntiPatternStore(nil, &mockAntiPatternBackend{})
	es := &store.ExperienceStore{AntiPatternStore: apStore}
	engine := &DirectedEngine{expStore: es}
	got := engine.buildAntiPatternGuidance("", "")
	if got != "" {
		t.Fatalf("expected empty guidance for empty intent, got %q", got)
	}
}

func TestBuildAntiPatternGuidance_MatchingPrecedent_FormatsBlock(t *testing.T) {
	backend := &mockAntiPatternBackend{}
	apStore := store.NewAntiPatternStore(nil, backend)
	apStore.Upsert(context.Background(), store.AntiPatternPrecedent{
		ID:             "prec_test_large_file",
		AntiPattern:    "Using patch tool on large files",
		CorrectPattern: "Use read_file with offset/limit + write_file",
		Symptom:        "Anchor drift: patch cannot find old_string",
		Category:       "golang_development",
		Confidence:     0.9,
	})
	es := &store.ExperienceStore{AntiPatternStore: apStore}
	engine := &DirectedEngine{expStore: es}

	got := engine.buildAntiPatternGuidance("patch a large file", "anchor drift")
	if got == "" {
		t.Fatal("expected non-empty guidance for matching precedent")
	}
	if !strings.Contains(got, "[PREVIOUS FAILURE ANTI-PATTERN]") {
		t.Errorf("guidance missing header marker; got:\n%s", got)
	}
	if !strings.Contains(got, "ANTI-PATTERN: Using patch tool on large files") {
		t.Errorf("guidance missing anti-pattern line; got:\n%s", got)
	}
	if !strings.Contains(got, "CORRECT PATTERN: Use read_file with offset/limit + write_file") {
		t.Errorf("guidance missing correct-pattern line; got:\n%s", got)
	}
	if !strings.Contains(got, "SYMPTOM: Anchor drift") {
		t.Errorf("guidance missing symptom line; got:\n%s", got)
	}
	if !strings.Contains(got, "END [PREVIOUS FAILURE ANTI-PATTERN]") {
		t.Errorf("guidance missing end marker; got:\n%s", got)
	}
}

func TestBuildAntiPatternGuidance_CappedAtMaxInjection(t *testing.T) {
	backend := &mockAntiPatternBackend{}
	apStore := store.NewAntiPatternStore(nil, backend)
	for i := 0; i < 6; i++ {
		apStore.Upsert(context.Background(), store.AntiPatternPrecedent{
			ID:          "prec_" + string(rune('a'+i)),
			AntiPattern: "antipattern " + string(rune('a'+i)),
			Category:    "test",
			Confidence:  0.5,
		})
	}
	es := &store.ExperienceStore{AntiPatternStore: apStore}
	engine := &DirectedEngine{expStore: es}

	got := engine.buildAntiPatternGuidance("matching query", "")
	count := strings.Count(got, "ANTI-PATTERN:")
	if count > maxAntiPatternInjection {
		t.Errorf("injected %d precedents, cap is %d; got:\n%s", count, maxAntiPatternInjection, got)
	}
}

// ── composeRecoveryPrompt ──────────────────────────────────────────────────

func TestComposeRecoveryPrompt_AllSignals(t *testing.T) {
	existing := "prior ODFTP recovery seed"
	reason := "output missing required field: version"
	antiPattern := "[PREVIOUS FAILURE ANTI-PATTERN]\nfoo\nEND [PREVIOUS FAILURE ANTI-PATTERN]\n"

	got := composeRecoveryPrompt(existing, reason, antiPattern)

	if !strings.Contains(got, existing) {
		t.Error("existing context should be preserved at the top")
	}
	if !strings.Contains(got, "REJECTION REASON: "+reason) {
		t.Error("rejection reason should be present")
	}
	if !strings.Contains(got, antiPattern) {
		t.Error("anti-pattern guidance should be appended after rejection")
	}
	idxExisting := strings.Index(got, existing)
	idxRejection := strings.Index(got, "REJECTION REASON:")
	idxAntiPattern := strings.Index(got, "[PREVIOUS FAILURE ANTI-PATTERN]")
	if !(idxExisting < idxRejection && idxRejection < idxAntiPattern) {
		t.Errorf("ordering wrong: existing=%d rejection=%d antipattern=%d", idxExisting, idxRejection, idxAntiPattern)
	}
}

func TestComposeRecoveryPrompt_NoExistingContext(t *testing.T) {
	got := composeRecoveryPrompt("", "fail reason", "")
	if strings.Contains(got, "REJECTION REASON: fail reason") == false {
		t.Error("rejection reason must always be present")
	}
	if strings.Contains(got, "[PREVIOUS FAILURE ANTI-PATTERN]") {
		t.Error("anti-pattern marker should be absent when guidance is empty")
	}
}

func TestComposeRecoveryPrompt_NoAntiPattern_OnlyRejection(t *testing.T) {
	got := composeRecoveryPrompt("", "schema violation", "")
	if !strings.Contains(got, "REJECTION REASON: schema violation") {
		t.Error("rejection reason should be present")
	}
	if strings.Contains(got, "[PREVIOUS FAILURE ANTI-PATTERN]") {
		t.Error("no anti-pattern marker expected")
	}
}

func TestComposeRecoveryPrompt_PriorContextPreservedAcrossRefine(t *testing.T) {
	first := composeRecoveryPrompt("", "first failure", "")
	second := composeRecoveryPrompt(first, "second failure", "")
	if !strings.Contains(second, "REJECTION REASON: first failure") {
		t.Error("first rejection must survive into the second refine iteration")
	}
	if !strings.Contains(second, "REJECTION REASON: second failure") {
		t.Error("second rejection must be present")
	}
}

// ── Module 1: ChatHarness Readiness Gate ───────────────────────────────────

func TestChatHarnessSystemPrompt_ContainsReadinessGate(t *testing.T) {
	h := &ChatHarness{}
	system := h.buildSystem("test query", "task1")
	if !strings.Contains(system, "DELEGATION READINESS GATE") {
		t.Error("system prompt must contain the Readiness Gate header")
	}
	if !strings.Contains(system, "NEVER blindly delegate a vague task") {
		t.Error("system prompt must contain the hard-rule phrasing")
	}
}

func TestChatHarnessDelegateTool_HasReadinessSelfCheck(t *testing.T) {
	h := &ChatHarness{}
	tools := h.toolDefinitions()
	var delegate *schemas.ToolDefinition
	for i := range tools {
		if tools[i].Name == "delegate_to_orchestrator" {
			delegate = &tools[i]
			break
		}
	}
	if delegate == nil {
		t.Fatal("delegate_to_orchestrator tool not found")
	}
	if !strings.Contains(delegate.Description, "READINESS GATE") {
		t.Error("delegate tool description must mention READINESS GATE")
	}
	props, ok := delegate.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("delegate tool schema missing properties")
	}
	if _, ok := props["readiness_self_check"]; !ok {
		t.Error("delegate tool schema must include readiness_self_check field")
	}
	promptSpec, _ := props["prompt"].(map[string]any)
	desc, _ := promptSpec["description"].(string)
	if !strings.Contains(desc, "MUST include") {
		t.Error("prompt description must specify the required components for readiness")
	}
}

// ── Integration: Auto-Refine loop injects anti-pattern on retry ────────────
//
// This is a focused integration test that exercises the real composeRecoveryPrompt
// + buildAntiPatternGuidance path with a real AntiPatternStore, simulating what
// the Auto-Refine loop in scheduler_submit.go does on a verifyExitCriteria failure.

func TestAutoRefine_InjectsAntiPatternOnRetry(t *testing.T) {
	backend := &mockAntiPatternBackend{}
	apStore := store.NewAntiPatternStore(nil, backend)
	apStore.Upsert(context.Background(), store.AntiPatternPrecedent{
		ID:             "prec_integration_anchor_drift",
		AntiPattern:    "Patching files over 1000 lines with patch tool",
		CorrectPattern: "Use read_file offset/limit + write_file overwrite",
		Symptom:        "Anchor drift repeated not found errors",
		Category:       "golang_development",
		Confidence:     0.95,
	})
	es := &store.ExperienceStore{AntiPatternStore: apStore}

	tmpDir, _ := os.MkdirTemp("", "autorefine")
	defer os.RemoveAll(tmpDir)
	logger, _ := NewLogger(tmpDir, nil)
	defer logger.Close()

	engine := &DirectedEngine{expStore: es, logger: logger}

	stepTask := "patch a large Go file to add a new method"
	rejectionReason := "FAIL: LOGIC: anchor drift, old_string not found in file"

	guidance := engine.buildAntiPatternGuidance(stepTask, rejectionReason)
	if guidance == "" {
		t.Fatal("expected anti-pattern guidance for matching task")
	}

	recovery := composeRecoveryPrompt("", rejectionReason, guidance)

	if !strings.Contains(recovery, "REJECTION REASON: "+rejectionReason) {
		t.Error("recovery prompt must contain the specific rejection reason")
	}
	if !strings.Contains(recovery, "[PREVIOUS FAILURE ANTI-PATTERN]") {
		t.Error("recovery prompt must contain the anti-pattern injection block")
	}
	if !strings.Contains(recovery, "Patching files over 1000 lines") {
		t.Error("recovery prompt must contain the matched historical anti-pattern")
	}
	if !strings.Contains(recovery, "Use read_file offset/limit + write_file overwrite") {
		t.Error("recovery prompt must contain the correct-pattern guidance")
	}
}
