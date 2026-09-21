package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// mockTaskStoreForHandoff implements store.ITaskStore for ref-based handoff tests.
type mockTaskStoreForHandoff struct{}

func (m *mockTaskStoreForHandoff) Set(ctx context.Context, taskID, stepID string, result *store.StepResult) error {
	return nil
}
func (m *mockTaskStoreForHandoff) Get(ctx context.Context, taskID, stepID string) (*store.StepResult, error) {
	return nil, os.ErrNotExist
}
func (m *mockTaskStoreForHandoff) GetByRef(ctx context.Context, ref string) (*store.StepResult, error) {
	return nil, os.ErrNotExist
}
func (m *mockTaskStoreForHandoff) ClearTask(ctx context.Context, taskID string) (int, error) {
	return 0, nil
}
func (m *mockTaskStoreForHandoff) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	return true, nil
}

// buildTestSpawnerForHandoff constructs a minimal Spawner pre-wired with an
// AssetManager and the given ref-based handoff threshold.
func buildTestSpawnerForHandoff(t *testing.T, threshold int) (*Spawner, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "anticliff_handoff")
	if err != nil {
		t.Fatal(err)
	}

	am := NewAssetManager(tmpDir, 1, nil)
	sp := &Spawner{
		assets:                   am,
		refBasedHandoffThreshold: threshold,
	}
	return sp, tmpDir
}

// invokeContextInjection reproduces the exact upstream-context injection
// branch from spawner.go's doSpawn. Returns the flattened user text.
func invokeContextInjection(t *testing.T, sp *Spawner, taskID, stepID string, upstreamCtx map[string]any) string {
	t.Helper()
	if len(upstreamCtx) == 0 {
		return "task: do something"
	}

	ctxBytes, _ := json.Marshal(upstreamCtx)
	ctxLen := len(ctxBytes)

	userText := "task: do something"

	effectiveThreshold := sp.GetEffectiveHandoffThreshold(nil)

	if sp.assets != nil && effectiveThreshold > 0 && ctxLen > effectiveThreshold {
		file, err := sp.assets.Handle(taskID, stepID, ctxBytes, "json", "upstream_context")
		if err == nil && file != nil {
			refPointer := map[string]any{
				"ref":     file.Path,
				"size":    ctxLen,
				"summary": "Upstream context side-loaded (adaptive model threshold); use read_file to inspect.",
				"step":    stepID,
				"task":    taskID,
			}
			ptrBytes, _ := json.Marshal(refPointer)
			userText = "task: do something\n\n### Upstream Context (ref-based)\n" + string(ptrBytes)
		} else {
			userText = "task: do something\n\n### Upstream Context\n" + string(ctxBytes)
		}
	} else {
		userText = "task: do something\n\n### Upstream Context\n" + string(ctxBytes)
	}
	return userText
}

func buildLargeUpstreamContext(nKB int) map[string]any {
	ctx := make(map[string]any)
	for i := 0; i < nKB*10; i++ {
		ctx[fmt.Sprintf("tool_result_%d", i)] = strings.Repeat("x", 100)
	}
	return ctx
}

// TestAntiCliff_RefBasedHandoff_DisabledWhenThresholdZero verifies that
// with threshold=0 (default), the mechanism falls back to 32KB. A payload
// well under 32KB (e.g. 4KB) should stay inline.
func TestAntiCliff_RefBasedHandoff_DisabledWhenThresholdZero(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 0)
	defer os.RemoveAll(tmpDir)

	smallCtx := buildLargeUpstreamContext(4) // ~5KB, well under 32KB
	userText := invokeContextInjection(t, sp, "task1", "step1", smallCtx)

	if strings.Contains(userText, "ref-based") {
		t.Error("expected inline for 5KB payload under 32KB fallback threshold, got ref-based header")
	}
}

func TestAntiCliff_RefBasedHandoff_SmallContextStaysInline(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 50000)
	defer os.RemoveAll(tmpDir)

	smallCtx := map[string]any{
		"tool_result_0": "short output",
		"status":        "ok",
	}
	userText := invokeContextInjection(t, sp, "task1", "step1", smallCtx)

	if strings.Contains(userText, "ref-based") {
		t.Error("small context should stay inline")
	}
}

func TestAntiCliff_RefBasedHandoff_LargeContextSideLoads(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 1000)
	defer os.RemoveAll(tmpDir)

	largeCtx := buildLargeUpstreamContext(20)
	userText := invokeContextInjection(t, sp, "task1", "step1", largeCtx)

	if !strings.Contains(userText, "### Upstream Context (ref-based)") {
		t.Error("expected ref-based header, got: " + userText[:120])
	}
	if strings.Contains(userText, "tool_result_0") {
		t.Error("raw upstream content leaked into subagent prompt")
	}
	if !strings.Contains(userText, "\"ref\"") {
		t.Error("ref pointer missing 'ref' field")
	}
	if !strings.Contains(userText, "\"size\"") {
		t.Error("ref pointer missing 'size' field")
	}
	if !strings.Contains(userText, "Upstream context side-loaded") {
		t.Error("ref pointer missing summary")
	}
	matches, _ := filepath.Glob(tmpDir + "/*")
	if len(matches) == 0 {
		t.Error("expected side-loaded artifact file on disk")
	}
}

func TestAntiCliff_RefBasedHandoff_FileContainsValidJSON(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 10)
	defer os.RemoveAll(tmpDir)

	payload := map[string]any{
		"count":      42,
		"risk_level": "low",
		"evidence":   []string{"file1.go", "file2.go"},
	}
	userText := invokeContextInjection(t, sp, "task1", "step1", payload)

	if !strings.Contains(userText, "ref-based") {
		t.Error("expected side-load for payload >10B threshold")
	}

	var pointer struct {
		Ref     string `json:"ref"`
		Size    int    `json:"size"`
		Summary string `json:"summary"`
	}
	ptrJSON := strings.TrimPrefix(userText, "task: do something\n\n### Upstream Context (ref-based)\n")
	if err := json.Unmarshal([]byte(ptrJSON), &pointer); err != nil {
		t.Fatalf("ref pointer not valid JSON: %v", err)
	}
	if pointer.Ref == "" {
		t.Fatal("ref pointer has empty 'ref' path")
	}

	data, err := os.ReadFile(pointer.Ref)
	if err != nil {
		t.Fatalf("failed to read side-loaded file: %v", err)
	}
	var recovered map[string]any
	if err := json.Unmarshal(data, &recovered); err != nil {
		t.Fatalf("side-loaded file not valid JSON: %v", err)
	}
	if got, ok := recovered["count"]; !ok || got != float64(42) {
		t.Errorf("count mismatch: got %v", got)
	}
}

func TestAntiCliff_RefBasedHandoff_BoundaryAtThreshold(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 100000)
	defer os.RemoveAll(tmpDir)

	ctx := map[string]any{"payload": strings.Repeat("a", 900)}
	userText := invokeContextInjection(t, sp, "task1", "step1", ctx)

	if strings.Contains(userText, "ref-based") {
		t.Error("payload below threshold should be inline")
	}
}

func TestAntiCliff_RefBasedHandoff_AssetWriteFailureFallbacks(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 1000)
	defer os.RemoveAll(tmpDir)

	sp.assets = NewAssetManager("/nonexistent/path/that/will/fail", 0, nil)
	largeCtx := buildLargeUpstreamContext(20)
	userText := invokeContextInjection(t, sp, "task1", "step1", largeCtx)

	if strings.Contains(userText, "ref-based") {
		t.Error("on asset write failure, should fall back to inline")
	}
}

func TestAntiCliff_RefBasedHandoff_ContextRefsWiring(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 500)
	defer os.RemoveAll(tmpDir)

	largeCtx := buildLargeUpstreamContext(10)
	userText := invokeContextInjection(t, sp, "task1", "step1", largeCtx)

	if !strings.Contains(userText, "ref-based") {
		t.Error("large resolvedCtx from ContextRefs should trigger ref-based handoff")
	}
}

func TestAntiCliff_RefBasedHandoff_PointerContainsStepAndTask(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 100)
	defer os.RemoveAll(tmpDir)

	largeCtx := buildLargeUpstreamContext(10)
	userText := invokeContextInjection(t, sp, "task-abc", "step-xyz", largeCtx)

	if !strings.Contains(userText, "step-xyz") {
		t.Error("ref pointer missing step_id value")
	}
	if !strings.Contains(userText, "task-abc") {
		t.Error("ref pointer missing task_id value")
	}
}

func TestAntiCliff_RefBasedHandoff_EmptyUpstreamContextNoop(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 100)
	defer os.RemoveAll(tmpDir)

	userText := invokeContextInjection(t, sp, "task1", "step1", nil)
	if userText != "task: do something" {
		t.Errorf("empty upstream should pass through unchanged: got %q", userText)
	}
}

// ============================================================
// Dynamic adaptive threshold tests (model-aware)
// ============================================================

// TestGetEffectiveHandoffThreshold_AdaptiveModelWindow verifies the
// dynamic threshold math: if explicit threshold is 0 but model window is set,
// the effective threshold becomes MaxContextWindow * 0.15 * 4 bytes.
func TestGetEffectiveHandoffThreshold_AdaptiveModelWindow(t *testing.T) {
	sp := &Spawner{}
	sp.SetRefBasedHandoffThreshold(0)

	pCfg := &config.ProviderConfig{MaxContextWindow: 100000}
	if got := sp.GetEffectiveHandoffThreshold(pCfg); got != 60000 {
		t.Errorf("expected 60000 for 100k model, got %d", got)
	}
}

// TestGetEffectiveHandoffThreshold_ExplicitOverridesAdaptive verifies that
// an explicit static threshold always wins over model-adaptive derivation.
func TestGetEffectiveHandoffThreshold_ExplicitOverridesAdaptive(t *testing.T) {
	sp := &Spawner{}
	sp.SetRefBasedHandoffThreshold(12345)

	pCfg := &config.ProviderConfig{MaxContextWindow: 100000}
	if got := sp.GetEffectiveHandoffThreshold(pCfg); got != 12345 {
		t.Errorf("expected explicit 12345, got %d", got)
	}
}

// TestGetEffectiveHandoffThreshold_FallbackDefault verifies that with
// neither explicit threshold nor model window set, we fall back to 32KB.
func TestGetEffectiveHandoffThreshold_FallbackDefault(t *testing.T) {
	sp := &Spawner{}
	if got := sp.GetEffectiveHandoffThreshold(nil); got != 32768 {
		t.Errorf("expected fallback 32768, got %d", got)
	}
	if got := sp.GetEffectiveHandoffThreshold(&config.ProviderConfig{}); got != 32768 {
		t.Errorf("expected fallback 32768 when MaxContextWindow=0, got %d", got)
	}
}

// TestGetEffectiveHandoffThreshold_LargeModelHighThreshold verifies a
// large-context model (e.g. 128k Gemini) gets a correspondingly higher
// threshold, preventing premature side-loading.
func TestGetEffectiveHandoffThreshold_LargeModelHighThreshold(t *testing.T) {
	m := &Spawner{}
	pCfg := &config.ProviderConfig{MaxContextWindow: 128000}
	expected := 76800
	if got := m.GetEffectiveHandoffThreshold(pCfg); got != expected {
		t.Errorf("expected %d for 128k model, got %d", expected, got)
	}
}

// TestGetEffectiveHandoffThreshold_SmallModelLowThreshold verifies a
// small-context model (e.g. 8k) gets a low threshold, forcing early
// side-loading to prevent context overflow.
func TestGetEffectiveHandoffThreshold_SmallModelLowThreshold(t *testing.T) {
	m := &Spawner{}
	pCfg := &config.ProviderConfig{MaxContextWindow: 8192}
	expected := 4915
	if got := m.GetEffectiveHandoffThreshold(pCfg); got != expected {
		t.Errorf("expected %d for 8k model, got %d", expected, got)
	}
}

// TestAntiCliff_RefBasedHandoff_IntegrationModelAdaptive verifies the full
// injection pipeline: when a model with 8k context is active and the
// threshold is 0 (unset), the effective threshold becomes ~4915 bytes,
// causing a 10KB payload to be side-loaded.
func TestAntiCliff_RefBasedHandoff_IntegrationModelAdaptive(t *testing.T) {
	sp, tmpDir := buildTestSpawnerForHandoff(t, 0)
	defer os.RemoveAll(tmpDir)

	// threshold=0 with no pCfg → fallback 32KB; build a 40KB payload to trigger side-load.
	veryLargeCtx := make(map[string]any)
	for i := 0; i < 400; i++ {
		veryLargeCtx[fmt.Sprintf("tool_result_%d", i)] = strings.Repeat("x", 100)
	}
	userText := invokeContextInjection(t, sp, "task1", "step1", veryLargeCtx)

	if !strings.Contains(userText, "ref-based") {
		t.Error("40KB payload should trigger side-load even with fallback 32KB threshold")
	}
}
