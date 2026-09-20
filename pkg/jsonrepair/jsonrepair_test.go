package jsonrepair

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRepair_AlreadyValid ensures valid JSON passes through unchanged.
func TestRepair_AlreadyValid(t *testing.T) {
	input := `{"name":"test","value":42}`
	out, ok := Repair(input)
	if !ok {
		t.Fatal("expected ok for valid JSON")
	}
	if out != input {
		t.Errorf("expected unchanged, got %q", out)
	}
}

// TestRepair_TruncatedObject repairs a JSON object whose last string value
// was cut mid-way by max_tokens.
func TestRepair_TruncatedString(t *testing.T) {
	input := `{"name":"John Doe","desc":"A tall man with long"`
	out, ok := Repair(input)
	if !ok {
		t.Fatalf("Repair failed; output: %q", out)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("repaired JSON still invalid: %v (output: %q)", err, out)
	}

	if m["name"] != "John Doe" {
		t.Errorf("name = %v, want John Doe", m["name"])
	}
	desc, _ := m["desc"].(string)
	if !strings.HasPrefix(desc, "A tall man") {
		t.Errorf("desc = %q, want prefix 'A tall man'", desc)
	}
}

// TestRepair_TruncatedNested repairs nested object + array truncation.
func TestRepair_TruncatedNested(t *testing.T) {
	input := `{"shots":[{"i":1,"s":"ok"},{"i":2,"s":"also ok"},{"i":3,"s":"trunca`
	out, ok := Repair(input)
	if !ok {
		t.Fatalf("Repair failed; output: %q", out)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("repaired JSON invalid: %v", err)
	}

	shots, _ := m["shots"].([]any)
	if len(shots) != 3 {
		t.Errorf("expected 3 shots (last one salvaged), got %d", len(shots))
	}
}

// TestRepair_TruncatedMidNumber repairs a number cut in the middle.
func TestRepair_TruncatedMidNumber(t *testing.T) {
	input := `{"count":42,"price":3.14`
	out, ok := Repair(input)
	if !ok {
		t.Fatalf("Repair failed; output: %q", out)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("repaired JSON invalid: %v", err)
	}

	if m["count"] != float64(42) {
		t.Errorf("count = %v, want 42", m["count"])
	}
}

// TestRepair_MarkdownWrapped handles JSON wrapped in markdown code fences
// before truncation — the caller should strip fences first, but Repair
// should still work on bare truncated JSON.
func TestRepair_MarkdownWrapped(t *testing.T) {
	// After stripping ```json prefix and ``` suffix (done by caller),
	// we get this truncated content:
	input := `{"shots":[{"i":1,"p":"a"},{"i":2,"p":"b"}`
	out, ok := Repair(input)
	if !ok {
		t.Fatalf("Repair failed; output: %q", out)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("repaired JSON invalid: %v", err)
	}
}

// TestTrimIncomplete_AlreadyValid passes valid arrays unchanged.
func TestTrimIncomplete_AlreadyValid(t *testing.T) {
	input := `[{"i":1},{"i":2},{"i":3}]`
	out, ok := TrimIncomplete(input)
	if !ok {
		t.Fatal("expected ok for valid JSON")
	}
	if out != input {
		t.Errorf("expected unchanged, got %q", out)
	}
}

// TestTrimIncomplete_DropsLastObject discards a half-finished trailing
// object from a JSON array — the core use case for storyboard shots.
func TestTrimIncomplete_DropsLastObject(t *testing.T) {
	input := `[{"i":1,"s":"ok"},{"i":2,"s":"ok"},{"i":3,"s":"trunca`
	out, ok := TrimIncomplete(input)
	if !ok {
		t.Fatalf("TrimIncomplete failed; output: %q", out)
	}

	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("repaired JSON invalid: %v (output: %q)", err, out)
	}

	if len(arr) != 2 {
		t.Errorf("expected 2 complete objects, got %d", len(arr))
	}

	if arr[0]["s"] != "ok" {
		t.Errorf("arr[0].s = %v, want 'ok'", arr[0]["s"])
	}
}

// TestTrimIncomplete_TruncatedAfterComma handles truncation right after
// a comma separating array elements.
func TestTrimIncomplete_TruncatedAfterComma(t *testing.T) {
	input := `[{"i":1},{"i":2},`
	out, ok := TrimIncomplete(input)
	if !ok {
		t.Fatalf("TrimIncomplete failed; output: %q", out)
	}

	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("repaired JSON invalid: %v", err)
	}

	if len(arr) != 2 {
		t.Errorf("expected 2 objects, got %d", len(arr))
	}
}

// TestTrimIncomplete_SingleObjectTruncated handles an array with only
// one object that is truncated.
func TestTrimIncomplete_SingleObjectTruncated(t *testing.T) {
	input := `[{"i":1,"s":"trunca`
	out, ok := TrimIncomplete(input)
	// With only one truncated object, we have no complete objects.
	// TrimIncomplete should fall back to Repair and try to salvage it.
	if !ok {
		// Acceptable: if neither strategy works, return false.
		// But Repair should close the string and brackets, so it should work.
		t.Fatalf("TrimIncomplete failed; output: %q", out)
	}

	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		// If it's not an array, maybe Repair returned an object — that's
		// acceptable for a single truncated element.
		var obj map[string]any
		if err2 := json.Unmarshal([]byte(out), &obj); err2 != nil {
			t.Fatalf("repaired JSON neither array nor object: %v (output: %q)", err, out)
		}
		// Single object is acceptable.
		return
	}
}

// TestRepair_EmptyString returns false for empty input.
func TestRepair_EmptyString(t *testing.T) {
	_, ok := Repair("")
	if ok {
		t.Error("expected false for empty string")
	}
}

// TestRepair_TrailingComma handles trailing comma before truncation point.
func TestRepair_TrailingComma(t *testing.T) {
	input := `{"a":1,"b":2,`
	out, ok := Repair(input)
	if !ok {
		t.Fatalf("Repair failed; output: %q", out)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("repaired JSON invalid: %v (output: %q)", err, out)
	}

	if m["a"] != float64(1) {
		t.Errorf("a = %v, want 1", m["a"])
	}
	if m["b"] != float64(2) {
		t.Errorf("b = %v, want 2", m["b"])
	}
}

// TestRepair_RealWorldStoryboard simulates an actual LLM storyboard output
// truncated at max_tokens=4096 with 8 shots, cut mid-way through shot 7.
func TestRepair_RealWorldStoryboard(t *testing.T) {
	// 6 complete shots + 1 truncated
	input := `{"shots":[
{"shot_index":1,"scene_desc":"森林清晨","characters":["小树苗"],"action":"生长","camera":"特写","dialogue":"","prompt_en":"A small green sprout growing in a sunlit forest, morning dew, close-up shot"},
{"shot_index":2,"scene_desc":"阳光照射","characters":["小树苗"],"action":"伸展叶子","camera":"中景","dialogue":"","prompt_en":"The sprout extends its leaves toward the sunlight"},
{"shot_index":3,"scene_desc":"暴雨来临","characters":["小树苗"],"action":"颤抖","camera":"远景","dialogue":"","prompt_en":"Dark clouds gather, rain starts falling on the small plant"},
{"shot_index":4,"scene_desc":"雨后","characters":["小树苗"],"action":"挺立","camera":"俯拍","dialogue":"","prompt_en":"After the storm the plant stands tall, water drops on leaves"},
{"shot_index":5,"scene_desc":"秋天","characters":["大树"],"action":"落叶","camera":"全景","dialogue":"","prompt_en":"A tall tree with golden leaves falling in autumn wind"},
{"shot_index":6,"scene_desc":"冬天","characters":["大树"],"action":"积雪","camera":"远景","dialogue":"","prompt_en":"The old tree covered in snow, winter landscape"},
{"shot_index":7,"scene_desc":"春天","characters":["大树"],"action":"发芽","camera":"特写","dialogue":"又一年春天了","prompt_en":"Spring returns, new green buds emerge`

	out, ok := Repair(input)
	if !ok {
		t.Fatalf("Repair failed; output length: %d", len(out))
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("repaired JSON invalid: %v", err)
	}

	shots, _ := result["shots"].([]any)
	// Repair should salvage all 7 (the last one gets its string closed).
	if len(shots) < 6 {
		t.Errorf("expected at least 6 shots, got %d", len(shots))
	}
}

// TestTrimIncomplete_RealWorldStoryboard same scenario but using
// TrimIncomplete which should cleanly drop shot 7.
func TestTrimIncomplete_RealWorldStoryboard(t *testing.T) {
	input := `{"shots":[
{"shot_index":1,"scene_desc":"森林清晨","prompt_en":"A small green sprout growing"},
{"shot_index":2,"scene_desc":"阳光照射","prompt_en":"The sprout extends leaves"},
{"shot_index":3,"scene_desc":"暴雨来临","prompt_en":"Dark clouds gather"},
{"shot_index":4,"scene_desc":"雨后","prompt_en":"After the storm"},
{"shot_index":5,"scene_desc":"秋天","prompt_en":"Golden leaves falling"},
{"shot_index":6,"scene_desc":"冬天","prompt_en":"Snow on tree"},
{"shot_index":7,"scene_desc":"春天","prompt_en":"Spring returns, new green buds`

	out, ok := TrimIncomplete(input)
	if !ok {
		t.Fatalf("TrimIncomplete failed; output length: %d", len(out))
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("repaired JSON invalid: %v", err)
	}

	shots, _ := result["shots"].([]any)
	if len(shots) != 6 {
		t.Errorf("expected exactly 6 shots (7th dropped), got %d", len(shots))
	}
}
