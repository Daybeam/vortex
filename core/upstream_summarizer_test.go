package core

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// ── Tests 1-4: DeterministicFactExtractor extraction rules ──────────────────
// See docs/plan/UPSTREAM_CONTEXT_SUMMARIZER_DESIGN.md §3.4.

// TestDeterministicFactExtractor_SchemaOverview verifies that a JSON object
// payload produces a key/type/length list (design §2.3 Rule 1).
func TestDeterministicFactExtractor_SchemaOverview(t *testing.T) {
	payload := []byte(`{"count": 42, "risk_level": "low", "evidence": ["file1.go", "file2.go"], "meta": {"author": "agent"}}`)
	extractor := DeterministicFactExtractor{}

	summary, err := extractor.Summarize(context.Background(), payload, 2000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary == "" {
		t.Fatal("expected non-empty summary for JSON payload")
	}

	// Should contain key facts header.
	if !strings.Contains(summary, "[关键事实]") {
		t.Error("summary missing [关键事实] header")
	}
	// Should mention the keys.
	for _, key := range []string{"count", "risk_level", "evidence", "meta"} {
		if !strings.Contains(summary, key) {
			t.Errorf("summary missing key %q", key)
		}
	}
	// Should mention types.
	if !strings.Contains(summary, "number") {
		t.Error("summary missing type 'number' for count field")
	}
	if !strings.Contains(summary, "string") {
		t.Error("summary missing type 'string' for risk_level field")
	}
	if !strings.Contains(summary, "array") {
		t.Error("summary missing type 'array' for evidence field")
	}
	// Should mention field count.
	if !strings.Contains(summary, "4 个字段") {
		t.Error("summary missing field count")
	}
}

// TestDeterministicFactExtractor_KeyFields verifies that known semantic anchors
// (S2T, DecisionID, Goal:) have their full values extracted (design §2.3 Rule 2).
func TestDeterministicFactExtractor_KeyFields(t *testing.T) {
	payload := []byte(`{
		"S2T": "Step 2 to Step 3 transition plan",
		"DecisionID": "DEC-2026-10-28-001",
		"Goal:": "Refactor the authentication module to use JWT tokens",
		"noise_field": "unimportant data"
	}`)
	extractor := DeterministicFactExtractor{}

	summary, err := extractor.Summarize(context.Background(), payload, 2000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary == "" {
		t.Fatal("expected non-empty summary")
	}

	// Anchor fields should have their full values.
	if !strings.Contains(summary, "Step 2 to Step 3 transition plan") {
		t.Error("S2T anchor value not fully extracted")
	}
	if !strings.Contains(summary, "DEC-2026-10-28-001") {
		t.Error("DecisionID anchor value not fully extracted")
	}
	if !strings.Contains(summary, "Refactor the authentication module to use JWT tokens") {
		t.Error("Goal: anchor value not fully extracted")
	}
	// Should indicate anchor fields were found.
	if !strings.Contains(summary, "anchor fields") {
		t.Error("summary should indicate anchor fields were found")
	}
}

// TestDeterministicFactExtractor_Markdown verifies that Markdown text produces
// headings + key sentences (design §2.3 Rule 3).
func TestDeterministicFactExtractor_Markdown(t *testing.T) {
	payload := []byte(`# Architecture Overview

## Authentication Module

The auth module handles JWT token validation. See §2.3 for details.

## Database Layer

Connection pool size = 100. Timeout is 30 seconds.
`)
	extractor := DeterministicFactExtractor{}

	summary, err := extractor.Summarize(context.Background(), payload, 2000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary == "" {
		t.Fatal("expected non-empty summary for Markdown payload")
	}

	// Should contain headings.
	if !strings.Contains(summary, "Architecture Overview") {
		t.Error("summary missing heading 'Architecture Overview'")
	}
	if !strings.Contains(summary, "Authentication Module") {
		t.Error("summary missing heading 'Authentication Module'")
	}
	if !strings.Contains(summary, "Database Layer") {
		t.Error("summary missing heading 'Database Layer'")
	}
	// Should contain key sentence with § reference.
	if !strings.Contains(summary, "§2.3") {
		t.Error("summary missing § reference key sentence")
	}
}

// TestDeterministicFactExtractor_Code verifies that Go code produces function
// and type signatures (design §2.3 Rule 4).
func TestDeterministicFactExtractor_Code(t *testing.T) {
	payload := []byte(`package auth

type Token struct {
	Value string
	Exp   int64
}

type Validator interface {
	Validate(t *Token) bool
}

func GenerateToken(userID string) *Token {
	return &Token{}
}

func (v *jwtValidator) Validate(t *Token) bool {
	return true
}
`)
	extractor := DeterministicFactExtractor{}

	summary, err := extractor.Summarize(context.Background(), payload, 2000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary == "" {
		t.Fatal("expected non-empty summary for code payload")
	}

	// Should contain function signatures.
	if !strings.Contains(summary, "func GenerateToken()") {
		t.Error("summary missing func GenerateToken signature")
	}
	if !strings.Contains(summary, "func Validate()") {
		t.Error("summary missing func Validate signature")
	}
	// Should contain type signatures.
	if !strings.Contains(summary, "type Token struct") {
		t.Error("summary missing type Token struct signature")
	}
	// Should indicate symbol count.
	if !strings.Contains(summary, "symbols") {
		t.Error("summary should indicate symbol count")
	}
}

// ── Tests 5-7: applyRefBasedHandoff integration ─────────────────────────────

// TestApplyRefBasedHandoff_WithSummarizer verifies that when the summarizer is
// enabled, the injected text contains key facts from the semantic summary
// instead of the fixed placeholder (design §3.4 test 5).
func TestApplyRefBasedHandoff_WithSummarizer(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 100) // low threshold → triggers side-load
	defer os.RemoveAll(tmpDir)

	// Wire the summarizer with Enabled=true.
	sp.SetUpstreamSummarizer(DeterministicFactExtractor{}, config.UpstreamSummarizerConfig{
		Enabled:     true,
		Mode:        "deterministic",
		BudgetChars: 2000,
	})

	// Upstream context with anchor fields.
	upstreamCtx := map[string]any{
		"DecisionID": "DEC-2026-10-28-001",
		"Goal:":      "Implement semantic compression for upstream context",
		"status":     "approved",
		"count":      42,
	}

	userText := invokeContextInjection(t, sp, "task1", "step1", upstreamCtx)

	// Should have ref-based header.
	if !strings.Contains(userText, "### Upstream Context (ref-based)") {
		t.Error("expected ref-based header")
	}
	// Should contain key facts from the semantic summary (not the placeholder).
	if !strings.Contains(userText, "DEC-2026-10-28-001") {
		t.Error("semantic summary missing DecisionID anchor value")
	}
	if !strings.Contains(userText, "Implement semantic compression for upstream context") {
		t.Error("semantic summary missing Goal: anchor value")
	}
	// Should NOT contain the old placeholder string.
	if strings.Contains(userText, "Upstream context side-loaded (adaptive model threshold)") {
		t.Error("old placeholder should not appear when summarizer is enabled and succeeds")
	}
}

// TestApplyRefBasedHandoff_SummarizerFails verifies that when the summarizer
// returns an error, the system gracefully degrades to the placeholder behavior
// and the main flow is not blocked (design §3.4 test 6).
func TestApplyRefBasedHandoff_SummarizerFails(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 100)
	defer os.RemoveAll(tmpDir)

	// Wire a failing summarizer.
	sp.SetUpstreamSummarizer(failingSummarizer{}, config.UpstreamSummarizerConfig{
		Enabled:     true,
		Mode:        "deterministic",
		BudgetChars: 2000,
	})

	upstreamCtx := map[string]any{
		"DecisionID":  "DEC-2026-10-28-001",
		"status":      "approved",
		"description": "Testing graceful degradation when the summarizer fails",
		"count":       42,
	}

	userText := invokeContextInjection(t, sp, "task1", "step1", upstreamCtx)

	// Main flow should continue: ref-based header should still appear.
	if !strings.Contains(userText, "### Upstream Context (ref-based)") {
		t.Error("expected ref-based header despite summarizer failure")
	}
	// Should fall back to the placeholder string (graceful degradation).
	if !strings.Contains(userText, "Upstream context side-loaded (adaptive model threshold)") {
		t.Error("expected placeholder fallback on summarizer failure")
	}
	// Should still have the ref pointer.
	if !strings.Contains(userText, "\"ref\"") {
		t.Error("ref pointer missing on summarizer failure")
	}
}

// TestApplyRefBasedHandoff_Disabled verifies that when Enabled=false, the
// existing placeholder behavior is preserved with zero behavior change
// (design §3.4 test 7).
func TestApplyRefBasedHandoff_Disabled(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 100)
	defer os.RemoveAll(tmpDir)

	// Wire the summarizer but with Enabled=false (default).
	sp.SetUpstreamSummarizer(DeterministicFactExtractor{}, config.UpstreamSummarizerConfig{
		Enabled: false,
	})

	upstreamCtx := map[string]any{
		"DecisionID": "DEC-2026-10-28-001",
		"Goal:":      "This should NOT appear in the summary",
		"status":     "approved",
	}

	userText := invokeContextInjection(t, sp, "task1", "step1", upstreamCtx)

	// Should have ref-based header.
	if !strings.Contains(userText, "### Upstream Context (ref-based)") {
		t.Error("expected ref-based header")
	}
	// Should contain the old placeholder string (existing behavior).
	if !strings.Contains(userText, "Upstream context side-loaded (adaptive model threshold)") {
		t.Error("expected placeholder string when summarizer is disabled")
	}
	// Should NOT contain semantic summary content.
	if strings.Contains(userText, "This should NOT appear in the summary") {
		t.Error("semantic summary should not be injected when disabled")
	}
}

// ── Test helpers ────────────────────────────────────────────────────────────

// failingSummarizer is a test double that always returns an error,
// used to verify graceful degradation.
type failingSummarizer struct{}

func (failingSummarizer) Summarize(ctx context.Context, payload []byte, budget int) (string, error) {
	return "", context.DeadlineExceeded
}
