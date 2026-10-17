package core

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// ── NewOutputProcessor constructor tests ───────────────────────────────────

func TestNewOutputProcessor_ExplicitValue(t *testing.T) {
	p := NewOutputProcessor(5000)
	if p.MaxBodyChars != 5000 {
		t.Errorf("explicit 5000: got %d", p.MaxBodyChars)
	}
}

func TestNewOutputProcessor_ZeroFallback(t *testing.T) {
	p := NewOutputProcessor(0)
	if p.MaxBodyChars != defaultMaxBodyChars {
		t.Errorf("zero should fall back to default %d, got %d", defaultMaxBodyChars, p.MaxBodyChars)
	}
}

func TestNewOutputProcessor_NegativeFallback(t *testing.T) {
	p := NewOutputProcessor(-1)
	if p.MaxBodyChars != defaultMaxBodyChars {
		t.Errorf("negative should fall back to default %d, got %d", defaultMaxBodyChars, p.MaxBodyChars)
	}
}

// Regression: pre-2026-10-17 behavior was hardcoded 3000. This test locks
// the fallback floor so existing configs without MaxToolResultChars and
// without MaxContextWindow get zero behavior change.
func TestNewOutputProcessor_LegacyFallback3000(t *testing.T) {
	p := NewOutputProcessor(0)
	if p.MaxBodyChars != 3000 {
		t.Errorf("legacy fallback must be 3000 for zero-change, got %d", p.MaxBodyChars)
	}
}

// ── SetMaxBodyChars lazy update tests ──────────────────────────────────────

func TestSetMaxBodyChars_UpdatesValue(t *testing.T) {
	p := NewOutputProcessor(3000)
	p.SetMaxBodyChars(16000)
	if p.MaxBodyChars != 16000 {
		t.Errorf("expected 16000 after SetMaxBodyChars, got %d", p.MaxBodyChars)
	}
}

func TestSetMaxBodyChars_IgnoresZero(t *testing.T) {
	p := NewOutputProcessor(8000)
	p.SetMaxBodyChars(0) // should not update
	if p.MaxBodyChars != 8000 {
		t.Errorf("SetMaxBodyChars(0) should not change value, got %d", p.MaxBodyChars)
	}
}

func TestSetMaxBodyChars_IgnoresNegative(t *testing.T) {
	p := NewOutputProcessor(8000)
	p.SetMaxBodyChars(-1)
	if p.MaxBodyChars != 8000 {
		t.Errorf("SetMaxBodyChars(-1) should not change value, got %d", p.MaxBodyChars)
	}
}

// ── computeDynamicMaxBodyChars formula tests ───────────────────────────────
//
// Formula: clamp(contextWindow × 0.08 × 4, 4000, 32000)
//   8K   → 8000  × 0.08 × 4 = 2560  → clamped to 4000  (min floor)
//   32K  → 32000 × 0.08 × 4 = 10240 → 10240
//   64K  → 64000 × 0.08 × 4 = 20480 → 20480
//   128K → 128000 × 0.08 × 4 = 40960 → clamped to 32000 (max ceiling)

func TestComputeDynamicMaxBodyChars_8K(t *testing.T) {
	v := computeDynamicMaxBodyChars(8000)
	if v != 4000 {
		t.Errorf("8K context: expected 4000 (min floor), got %d", v)
	}
}

func TestComputeDynamicMaxBodyChars_32K(t *testing.T) {
	v := computeDynamicMaxBodyChars(32000)
	if v != 10240 {
		t.Errorf("32K context: expected 10240, got %d", v)
	}
}

func TestComputeDynamicMaxBodyChars_128K(t *testing.T) {
	v := computeDynamicMaxBodyChars(128000)
	if v != 32000 {
		t.Errorf("128K context: expected 32000 (max ceiling), got %d", v)
	}
}

func TestComputeDynamicMaxBodyChars_ZeroFallback(t *testing.T) {
	v := computeDynamicMaxBodyChars(0)
	if v != defaultMaxBodyChars {
		t.Errorf("zero context: expected fallback %d, got %d", defaultMaxBodyChars, v)
	}
}

func TestComputeDynamicMaxBodyChars_NegativeFallback(t *testing.T) {
	v := computeDynamicMaxBodyChars(-1)
	if v != defaultMaxBodyChars {
		t.Errorf("negative context: expected fallback %d, got %d", defaultMaxBodyChars, v)
	}
}

// ── getMaxToolResultChars resolution priority tests ────────────────────────

func TestGetMaxToolResultChars_NilRegistry_Fallback(t *testing.T) {
	v := getMaxToolResultChars(nil)
	if v != defaultMaxBodyChars {
		t.Errorf("nil registry: expected fallback %d, got %d", defaultMaxBodyChars, v)
	}
}

func TestGetMaxToolResultChars_ExplicitConfig(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			MaxToolResultChars: 12000,
		},
	}
	v := getMaxToolResultChars(reg)
	if v != 12000 {
		t.Errorf("explicit config 12000: got %d", v)
	}
}

func TestGetMaxToolResultChars_DynamicFromProvider(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "test",
		Providers: map[string]*config.ProviderConfig{
			"test": {
				Model:           "test-model",
				MaxContextWindow: 128000,
			},
		},
	}
	v := getMaxToolResultChars(reg)
	// 128K → 32000 (max ceiling)
	if v != 32000 {
		t.Errorf("128K provider: expected 32000, got %d", v)
	}
}

func TestGetMaxToolResultChars_DynamicFromProvider_32K(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "test",
		Providers: map[string]*config.ProviderConfig{
			"test": {
				Model:           "test-model",
				MaxContextWindow: 32000,
			},
		},
	}
	v := getMaxToolResultChars(reg)
	if v != 10240 {
		t.Errorf("32K provider: expected 10240, got %d", v)
	}
}

func TestGetMaxToolResultChars_NoProvider_Fallback(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "",
	}
	v := getMaxToolResultChars(reg)
	if v != defaultMaxBodyChars {
		t.Errorf("no provider: expected fallback %d, got %d", defaultMaxBodyChars, v)
	}
}

func TestGetMaxToolResultChars_ProviderNoContextWindow_Fallback(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "test",
		Providers: map[string]*config.ProviderConfig{
			"test": {
				Model:            "test-model",
				MaxContextWindow: 0, // not set
			},
		},
	}
	v := getMaxToolResultChars(reg)
	if v != defaultMaxBodyChars {
		t.Errorf("provider without MaxContextWindow: expected fallback %d, got %d", defaultMaxBodyChars, v)
	}
}

// Explicit config takes priority over dynamic from provider.
func TestGetMaxToolResultChars_ExplicitOverridesDynamic(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "test",
		Providers: map[string]*config.ProviderConfig{
			"test": {
				Model:           "test-model",
				MaxContextWindow: 128000,
			},
		},
		System: config.SystemSettings{
			MaxToolResultChars: 5000, // explicit, should win over 32000 dynamic
		},
	}
	v := getMaxToolResultChars(reg)
	if v != 5000 {
		t.Errorf("explicit 5000 should override dynamic 32000, got %d", v)
	}
}

// ── Truncation behavior tests ──────────────────────────────────────────────

func TestTruncate_BelowLimit_NoTruncation(t *testing.T) {
	p := NewOutputProcessor(100)
	input := "short string"
	result := p.truncate(input)
	if result != input {
		t.Errorf("input below limit should not be truncated")
	}
}

func TestTruncate_AboveLimit_HeadTailSplit(t *testing.T) {
	p := NewOutputProcessor(100)
	input := strings.Repeat("A", 300) // 300 chars, limit 100
	result := p.truncate(input)
	if !strings.Contains(result, "[TRUNCATED") {
		t.Errorf("truncated output should contain TRUNCATED marker")
	}
	// Head should be first 50 chars, tail should be last 50 chars
	if !strings.HasPrefix(result, strings.Repeat("A", 50)) {
		t.Errorf("head should be first 50 chars")
	}
	if !strings.HasSuffix(result, strings.Repeat("A", 50)) {
		t.Errorf("tail should be last 50 chars")
	}
}

func TestTruncate_DynamicLimit_LargerBudget(t *testing.T) {
	// With a dynamic limit of 32000, a 5000-char input should NOT be truncated.
	p := NewOutputProcessor(32000)
	input := strings.Repeat("B", 5000)
	result := p.truncate(input)
	if result != input {
		t.Errorf("5000-char input under 32000 limit should not be truncated")
	}
}

// ── JSON-aware truncation tests ────────────────────────────────────────────

func TestTruncateJSON_ValidJSON_PreservesStructure(t *testing.T) {
	// JSON with a large string field that exceeds MaxBodyChars.
	largeText := strings.Repeat("X", 5000)
	input := `{"name":"test","data":"` + largeText + `","count":42}`
	p := NewOutputProcessor(500) // small budget to force truncation
	result := p.truncate(input)

	// Result must be valid JSON (parseable).
	var obj map[string]any
	if err := json.Unmarshal([]byte(result), &obj); err != nil {
		t.Errorf("truncated result must be valid JSON, got parse error: %v\nResult: %s", err, result[:min(200, len(result))])
	}
	// Structure must be preserved: name and count fields still present.
	if obj["name"] != "test" {
		t.Errorf("name field should be preserved, got %v", obj["name"])
	}
	if obj["count"] != float64(42) {
		t.Errorf("count field should be preserved, got %v", obj["count"])
	}
}

func TestTruncateJSON_LargeStringField_Truncated(t *testing.T) {
	largeText := strings.Repeat("Y", 10000)
	input := `{"text":"` + largeText + `"}`
	p := NewOutputProcessor(500)
	result := p.truncate(input)

	var obj map[string]any
	if err := json.Unmarshal([]byte(result), &obj); err != nil {
		t.Fatalf("result must be valid JSON: %v", err)
	}
	text, ok := obj["text"].(string)
	if !ok {
		t.Fatalf("text field should be a string")
	}
	if len(text) >= len(largeText) {
		t.Errorf("large text field should be truncated: original=%d, result=%d", len(largeText), len(text))
	}
	if !strings.Contains(text, "[truncated") {
		t.Errorf("truncated field should contain [truncated marker")
	}
}

func TestTruncateJSON_NestedJSON_PreservesStructure(t *testing.T) {
	largeText := strings.Repeat("Z", 5000)
	input := `{"level1":{"level2":{"text":"` + largeText + `"},"count":5},"status":"ok"}`
	p := NewOutputProcessor(500)
	result := p.truncate(input)

	var obj map[string]any
	if err := json.Unmarshal([]byte(result), &obj); err != nil {
		t.Fatalf("nested JSON result must be valid: %v", err)
	}
	if obj["status"] != "ok" {
		t.Errorf("top-level status should be preserved")
	}
	level1, ok := obj["level1"].(map[string]any)
	if !ok {
		t.Fatalf("level1 should be a map")
	}
	if level1["count"] != float64(5) {
		t.Errorf("nested count should be preserved")
	}
}

func TestTruncateJSON_ArrayOfObjects_PreservesStructure(t *testing.T) {
	largeText := strings.Repeat("A", 3000)
	input := `{"results":[{"id":1,"text":"` + largeText + `"},{"id":2,"text":"short"}]}`
	p := NewOutputProcessor(500)
	result := p.truncate(input)

	var obj map[string]any
	if err := json.Unmarshal([]byte(result), &obj); err != nil {
		t.Fatalf("array JSON result must be valid: %v", err)
	}
	results, ok := obj["results"].([]any)
	if !ok {
		t.Fatalf("results should be an array")
	}
	if len(results) != 2 {
		t.Errorf("array length should be preserved: expected 2, got %d", len(results))
	}
}

func TestTruncateJSON_NonJSON_FallsBackToHeadTail(t *testing.T) {
	// Plain text that looks nothing like JSON.
	input := strings.Repeat("plain text line\n", 1000)
	p := NewOutputProcessor(500)
	result := p.truncate(input)

	// Should contain the TRUNCATED marker from head+tail fallback.
	if !strings.Contains(result, "TRUNCATED") {
		t.Errorf("non-JSON content should use head+tail fallback with TRUNCATED marker")
	}
}

func TestTruncateJSON_InvalidJSON_FallsBackToHeadTail(t *testing.T) {
	// Starts with { but is invalid JSON.
	input := `{"broken": ` + strings.Repeat("X", 5000)
	p := NewOutputProcessor(500)
	result := p.truncate(input)

	if !strings.Contains(result, "TRUNCATED") {
		t.Errorf("invalid JSON should fall back to head+tail with TRUNCATED marker")
	}
}

func TestTruncateJSON_BelowLimit_NoTruncation(t *testing.T) {
	input := `{"small":"value","count":1}`
	p := NewOutputProcessor(500)
	result := p.truncate(input)
	if result != input {
		t.Errorf("JSON under limit should not be truncated")
	}
}

func TestWalkAndTruncateStrings_ShortString_Unchanged(t *testing.T) {
	result := walkAndTruncateStrings("short", 100)
	if result != "short" {
		t.Errorf("short string should be unchanged")
	}
}

func TestWalkAndTruncateStrings_LongString_Truncated(t *testing.T) {
	long := strings.Repeat("Q", 1000)
	result := walkAndTruncateStrings(long, 100).(string)
	if len(result) >= 1000 {
		t.Errorf("long string should be truncated")
	}
	if !strings.Contains(result, "[truncated") {
		t.Errorf("truncated string should contain marker")
	}
}

func TestWalkAndTruncateStrings_NonString_Unchanged(t *testing.T) {
	if walkAndTruncateStrings(42, 100) != 42 {
		t.Errorf("int should be unchanged")
	}
	if walkAndTruncateStrings(true, 100) != true {
		t.Errorf("bool should be unchanged")
	}
	if walkAndTruncateStrings(nil, 100) != nil {
		t.Errorf("nil should be unchanged")
	}
}
