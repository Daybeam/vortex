package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// MockProvider implements providers.Provider for testing
type MockProvider struct {
	name string
}

func (m *MockProvider) Name() string { return m.name }
func (m *MockProvider) CountTokens(ctx context.Context, text string) (int, error) {
	// Simple heuristic for testing: 1 token per 4 chars
	return len(text) / 4, nil
}
func (m *MockProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return &schemas.ProviderResponse{Text: "test"}, nil
}
func (m *MockProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	return nil, nil
}
func (m *MockProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}

func TestSievePipeline(t *testing.T) {
	reg := &config.Registry{
		Providers: make(map[string]*config.ProviderConfig),
	}
	reg.Providers["gemini"] = &config.ProviderConfig{
		Provider:               "gemini",
		MaxContextWindow:       1000, // Low limit for testing
		EffectiveContextWindow: 500,
		TokenLimit:             1200,
	}

	cm := NewContextManager(reg)
	provider := &MockProvider{name: "gemini"}

	// 1. Construct a "Dirty" and "Overloaded" context
	systemPrompt := "You are an S2T Agent. Goal: Analyze NVDA. " + strings.Repeat("System State Snapshot: OK\n", 20)
	userPrompt := "User: Start analysis\n" +
		strings.Repeat("[2026-07-08 12:00:00] Processing data... Progress: 10%\n", 50) +
		"Assistant: I found some data.\n" +
		strings.Repeat("[2026-07-08 12:00:01] Fetching from API... Progress: 20%\n", 50) +
		"User: Continue\n" +
		"Assistant: Final Result: NVDA is Bullish."

	req := &schemas.CompleteRequest{
		System: systemPrompt,
		User:   userPrompt,
	}

	// 2. Audit
	res, err := cm.Audit(context.Background(), provider, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	fmt.Printf("Audit Decision: %v, Original Tokens: %d, Action: %s\n", res.Decision, res.OriginalTokens, res.ActionTaken)

	// 3. Squeeze
	squeezedReq, squeezeRes, err := cm.Squeeze(context.Background(), provider, req, res.Decision)
	if err != nil {
		t.Fatalf("Squeeze failed: %v", err)
	}
	fmt.Printf("Squeeze Result: Compressed Tokens: %d, Ratio: %.2f\n", squeezeRes.CompressedTokens, squeezeRes.CompressionRatio)

	// 4. Semantic Verification
	if !strings.Contains(squeezedReq.System, "Goal: Analyze NVDA") {
		t.Errorf("Critical goal lost in system prompt")
	}
	if !strings.Contains(squeezedReq.User, "Final Result: NVDA is Bullish") {
		t.Errorf("Critical result lost in user prompt")
	}
	if strings.Contains(squeezedReq.User, "Progress: 10%") {
		t.Errorf("Noise not removed by RTK-Filter")
	}
}

func TestAudit_UnconfiguredContextWindow_DoesNotFalsePositiveCritical(t *testing.T) {
	// Regression test for the 2026-07-12 fix: a provider with no
	// max_context_window/effective_context_window/token_limit configured
	// (the common case for most providers in config_windows.json -- gemini,
	// gemma, openai, deepseek-v4-flash, sensenova-*, local/ollama) must not
	// be treated as having a hard limit of 0 tokens. Before the fix, this
	// exact setup made Audit() classify every non-empty prompt as
	// LevelCritical/HARD_LIMIT_BREACH_FORK on turn 0, which cascaded into an
	// unconditional DYNAMIC_ESCALATION_REQUIRED failure for every task using
	// an unconfigured provider.
	reg := &config.Registry{
		Providers: make(map[string]*config.ProviderConfig),
	}
	reg.Providers["gemini"] = &config.ProviderConfig{
		Provider: "gemini",
		// Deliberately NOT setting MaxContextWindow/EffectiveContextWindow/
		// TokenLimit, matching config_windows.json's actual "gemini" entry.
	}

	cm := NewContextManager(reg)
	provider := &MockProvider{name: "gemini"}

	req := &schemas.CompleteRequest{
		System: "You are a helpful assistant.",
		User:   "Please do a short, ordinary task.",
	}

	res, err := cm.Audit(context.Background(), provider, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.Decision != LevelNone {
		t.Fatalf("expected LevelNone for an unconfigured context window, got %v (action: %s)", res.Decision, res.ActionTaken)
	}
}

func TestAudit_BrandingMatch(t *testing.T) {
	// Verify that Audit correctly matches the provider by its brand name
	// (e.g. "sensenova") rather than the technical implementation name ("openai").
	reg := &config.Registry{
		Providers: make(map[string]*config.ProviderConfig),
	}
	reg.Providers["sensenova-6.7-flash"] = &config.ProviderConfig{
		Provider:               "sensenova",
		MaxContextWindow:       1000,
		EffectiveContextWindow: 500,
		TokenLimit:             1200,
	}

	cm := NewContextManager(reg)
	provider := &MockProvider{name: "sensenova"}

	req := &schemas.CompleteRequest{
		System: "You are a helpful assistant.",
		User:   "Short task.",
	}

	res, err := cm.Audit(context.Background(), provider, req)
	if err != nil {
		t.Fatalf("Audit failed: %v", err)
	}
	if res.ActionTaken == "NO_LIMIT_CONFIGURED" {
		t.Errorf("Audit failed to match provider branding; got NO_LIMIT_CONFIGURED")
	}
}

func TestApplyRTK(t *testing.T) {
	input := "[2026-07-08 12:00:00] Start\nProgress: 50%\nProcessing something... 100%\nUpdating database...\nKeep this line."
	got := applyRTK(input)
	if strings.Contains(got, "[2026-07-08") {
		t.Error("Timestamp not removed")
	}
	if strings.Contains(got, "Progress:") {
		t.Error("Progress log not removed")
	}
	if strings.Contains(got, "Processing") {
		t.Error("Processing log not removed")
	}
	if !strings.Contains(got, "Keep this line.") {
		t.Error("Valid content lost")
	}
}

func TestApplyLogCompress_PreservesErrorsAndStackTraces(t *testing.T) {
	input := `[2026-07-08 12:00:00] Start task
[2026-07-08 12:00:01] INFO: Step 1
[2026-07-08 12:00:02] INFO: Step 2
[2026-07-08 12:00:03] INFO: Step 3
[2026-07-08 12:00:04] INFO: Step 4
[2026-07-08 12:00:05] INFO: Step 5
[2026-07-08 12:00:06] FATAL: Connection reset by peer
	at main.connectDB(db.go:42)
[2026-07-08 12:00:07] Progress: 99%`

	got := applyLogCompress(input)

	// FATAL and stack trace line must be preserved
	if !strings.Contains(got, "FATAL: Connection reset by peer") {
		t.Error("FATAL error line was lost")
	}
	if !strings.Contains(got, "at main.connectDB(db.go:42)") {
		t.Error("Stack trace line was lost")
	}
	// Timestamps on error line should be preserved
	if !strings.Contains(got, "[2026-07-08 12:00:06] FATAL") {
		t.Error("Timestamp on FATAL error line should be preserved")
	}
	// Progress line should be removed
	if strings.Contains(got, "Progress: 99%") {
		t.Error("Progress line should be removed")
	}
	// Repetitive INFO lines (>3) should be capped
	lines := strings.Split(got, "\n")
	infoCount := 0
	for _, l := range lines {
		if strings.Contains(l, "INFO: Step") {
			infoCount++
		}
	}
	if infoCount > 3 {
		t.Errorf("Expected at most 3 INFO lines, got %d", infoCount)
	}
}

func TestApplyJSONCompress_FoldsLargeArraysAndPreservesErrors(t *testing.T) {
	// Construct a JSON array of 10 items with 1 error item at index 5
	items := make([]string, 10)
	for i := 0; i < 10; i++ {
		if i == 5 {
			items[i] = `{"id": 5, "status": "failed", "error": "timeout"}`
		} else {
			items[i] = fmt.Sprintf(`{"id": %d, "status": "ok"}`, i)
		}
	}
	jsonInput := "[" + strings.Join(items, ",") + "]"

	got := applyJSONCompress(jsonInput)

	// Must contain boundary items (id 0, 1, 8, 9) and error item (id 5)
	if !strings.Contains(got, `"id":0`) && !strings.Contains(got, `"id": 0`) {
		t.Error("Head boundary item 0 lost")
	}
	if !strings.Contains(got, `"id":5`) && !strings.Contains(got, `"id": 5`) {
		t.Error("Error item 5 lost")
	}
	if !strings.Contains(got, `"_compressed_count":3`) && !strings.Contains(got, `"_compressed_count": 3`) {
		t.Errorf("Expected _compressed_count fold marker, got: %s", got)
	}
}

func TestApplyCodeCompress_FoldsLargeFunctionBodies(t *testing.T) {
	codeLines := []string{
		"package main",
		"import \"fmt\"",
		"func heavyCalculation() int {",
	}
	for i := 0; i < 20; i++ {
		codeLines = append(codeLines, fmt.Sprintf("\tval%d := %d * 2", i, i))
	}
	codeLines = append(codeLines, "\treturn 42", "}")

	input := strings.Join(codeLines, "\n")
	got := applyCodeCompress(input)

	if !strings.Contains(got, "func heavyCalculation() int {") {
		t.Error("Function signature lost")
	}
	if !strings.Contains(got, "/* ... 21 lines compressed ... */") {
		t.Errorf("Expected body compression marker, got: %s", got)
	}
}

func TestApplySessionDedup(t *testing.T) {
	longLine := strings.Repeat("A", 150)
	input := longLine + "\n" + longLine + "\nShort line.\n" + longLine
	got := applySessionDedup(input)
	count := strings.Count(got, "AAAA")
	if count > 150 { // Should only have one occurrence of longLine
		t.Errorf("Deduplication failed, found %d occurrences of long block", count/150)
	}
	if !strings.Contains(got, "Short line.") {
		t.Error("Short line lost")
	}
}

func TestApplySemanticPruning(t *testing.T) {
	// Phase 4: marker-agnostic paragraph dedup + head/tail preservation.
	// Input has no blank lines → line-level fallback. Duplicate lines ("Line 2",
	// "Line 3") should be deduped by sha256 hash.
	input := "User: Task 1\nLine 2\nLine 3\nAssistant: OK\nUser: Task 2\nLine 2\nLine 3\nUser: Task 3\nAssistant: OK 3\nUser: Task 4\nAssistant: OK 4\nUser: Task 5\nAssistant: Final result."
	got := applySemanticPruning(input)
	if !strings.Contains(got, "Task 1") {
		t.Error("Head (Task 1) lost")
	}
	if !strings.Contains(got, "Final result.") {
		t.Error("Tail lost")
	}
	// Dedup: "Line 2" should appear only once.
	if c := strings.Count(got, "Line 2"); c != 1 {
		t.Errorf("dedup failed: 'Line 2' appears %d times, want 1", c)
	}
	// Dedup: "Line 3" should appear only once.
	if c := strings.Count(got, "Line 3"); c != 1 {
		t.Errorf("dedup failed: 'Line 3' appears %d times, want 1", c)
	}
}

func TestApplySemanticPruning_ParagraphLevel(t *testing.T) {
	// Text with blank-line-separated paragraphs → paragraph-level path.
	// Middle paragraphs with > 3 lines should get "[compressed]" marker.
	paragraphs := []string{
		"Goal: analyze the dataset",
		"Context: the dataset has 1000 rows",
		"Step 1: load data\nline 2\nline 3\nline 4\nline 5",
		"Step 2: clean data\nline 2\nline 3\nline 4\nline 5",
		"Step 3: transform\nline 2\nline 3\nline 4\nline 5",
		"Step 4: analyze\nline 2\nline 3\nline 4\nline 5",
		"Step 5: visualize\nline 2\nline 3\nline 4\nline 5",
		"Result: analysis complete",
		"Conclusion: ready for review",
	}
	input := strings.Join(paragraphs, "\n\n")
	got := applySemanticPruning(input)

	// Head preserved.
	if !strings.Contains(got, "Goal: analyze") {
		t.Error("head paragraph lost")
	}
	// Tail preserved.
	if !strings.Contains(got, "Conclusion: ready for review") {
		t.Error("tail paragraph lost")
	}
	// Middle paragraphs with > 3 lines should be compressed.
	if !strings.Contains(got, "... [compressed]") {
		t.Error("middle paragraphs not compressed")
	}
	// Compressed paragraphs should not contain their 5th line.
	if strings.Contains(got, "line 5") {
		t.Error("compressed paragraph should not contain line 5")
	}
}

func TestApplySemanticPruning_DedupByHash(t *testing.T) {
	// Identical paragraphs should be deduped even if they appear far apart.
	paragraphs := []string{
		"Head paragraph",
		"Unique content A",
		"Repeated block\nline 2\nline 3",
		"Unique content B",
		"Repeated block\nline 2\nline 3", // duplicate
		"Unique content C",
		"Unique content D",
		"Tail paragraph",
	}
	input := strings.Join(paragraphs, "\n\n")
	got := applySemanticPruning(input)

	count := strings.Count(got, "Repeated block")
	if count != 1 {
		t.Errorf("dedup by hash failed: 'Repeated block' appears %d times, want 1", count)
	}
}

func TestApplySemanticPruning_TooShortToPrune(t *testing.T) {
	// <= 5 paragraphs → return unchanged.
	input := "Line 1\nLine 2\nLine 3\nLine 4\nLine 5"
	got := applySemanticPruning(input)
	if got != input {
		t.Errorf("expected unchanged for short input; got %q", got)
	}
}

func TestApplySemanticPruning_LowDensityDropped(t *testing.T) {
	// Middle paragraphs with low density (mostly whitespace) should be dropped.
	paragraphs := []string{
		"Head paragraph with content",
		"Unique content A",
		"   \n   \n   \n   ", // low density (mostly spaces)
		"Unique content B",
		"Unique content C",
		"Unique content D",
		"Unique content E",
		"Tail paragraph with content",
	}
	input := strings.Join(paragraphs, "\n\n")
	got := applySemanticPruning(input)

	// The low-density paragraph should be dropped.
	if strings.Contains(got, "   \n   \n") {
		t.Error("low-density paragraph should have been dropped")
	}
	// Head and tail should still be present.
	if !strings.Contains(got, "Head paragraph") {
		t.Error("head lost")
	}
	if !strings.Contains(got, "Tail paragraph") {
		t.Error("tail lost")
	}
}
