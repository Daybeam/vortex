package schemas

import (
	"encoding/json"
	"testing"
)

// TestParseOutput_StringResult is the regression test for the live-test stall
// fix (2026-10-29). The iFlytek 4B model returns "result": "Hello!" (a JSON
// string) but SubagentOutput.Result is map[string]any. Before the fix,
// json.Unmarshal failed and ParseOutput fell back to StatusPartial with
// Confidence=0.3, causing the engine to loop forever nudging the model.
//
// Bug: live test stall — engine never reaches terminal state because
// ParseOutput can't handle string result values from smaller LLMs.
func TestParseOutput_StringResult(t *testing.T) {
	raw := `{"status":"ok","confidence":0.9,"result":"Hello!"}`
	out := ParseOutput(raw, "text")

	if out.Status != StatusOK {
		t.Errorf("status: want ok, got %s", out.Status)
	}
	if out.Confidence != 0.9 {
		t.Errorf("confidence: want 0.9, got %f", out.Confidence)
	}
	if out.Result == nil {
		t.Fatal("result: expected non-nil map, got nil")
	}
	rawVal, ok := out.Result["raw"]
	if !ok {
		t.Fatalf("result: expected 'raw' key, got %v", out.Result)
	}
	if rawVal != "Hello!" {
		t.Errorf("result[raw]: want 'Hello!', got %v", rawVal)
	}
}

// TestParseOutput_ArrayResult verifies array result values are wrapped in
// {"raw": value} just like string results.
func TestParseOutput_ArrayResult(t *testing.T) {
	raw := `{"status":"ok","confidence":0.8,"result":[1,2,3]}`
	out := ParseOutput(raw, "text")

	if out.Status != StatusOK {
		t.Errorf("status: want ok, got %s", out.Status)
	}
	if out.Result == nil {
		t.Fatal("result: expected non-nil map, got nil")
	}
	rawVal, ok := out.Result["raw"]
	if !ok {
		t.Fatalf("result: expected 'raw' key, got %v", out.Result)
	}
	arr, ok := rawVal.([]any)
	if !ok {
		t.Fatalf("result[raw]: expected []any, got %T: %v", rawVal, rawVal)
	}
	if len(arr) != 3 {
		t.Errorf("result[raw] length: want 3, got %d", len(arr))
	}
}

// TestParseOutput_MapResultStillWorks verifies that the traditional
// "result": {"key": "value"} format still works after the fix.
func TestParseOutput_MapResultStillWorks(t *testing.T) {
	raw := `{"status":"ok","confidence":0.95,"result":{"answer":"42"}}`
	out := ParseOutput(raw, "text")

	if out.Status != StatusOK {
		t.Errorf("status: want ok, got %s", out.Status)
	}
	if out.Confidence != 0.95 {
		t.Errorf("confidence: want 0.95, got %f", out.Confidence)
	}
	if out.Result == nil {
		t.Fatal("result: expected non-nil map, got nil")
	}
	if out.Result["answer"] != "42" {
		t.Errorf("result[answer]: want '42', got %v", out.Result["answer"])
	}
}

// TestParseOutput_NumericResult verifies numeric result values are wrapped.
func TestParseOutput_NumericResult(t *testing.T) {
	raw := `{"status":"ok","confidence":0.9,"result":42}`
	out := ParseOutput(raw, "text")

	if out.Status != StatusOK {
		t.Errorf("status: want ok, got %s", out.Status)
	}
	rawVal, ok := out.Result["raw"]
	if !ok {
		t.Fatalf("result: expected 'raw' key, got %v", out.Result)
	}
	if rawVal != float64(42) {
		t.Errorf("result[raw]: want 42, got %v", rawVal)
	}
}

// TestParseWithFlexibleResult_HollowReturnsFalse verifies that hollow
// outputs (empty status, zero confidence, no result) are rejected.
func TestParseWithFlexibleResult_HollowReturnsFalse(t *testing.T) {
	tests := []string{
		`{}`,
		`{"status":"","confidence":0}`,
		`{"result":"hello"}`,
	}
	for _, raw := range tests {
		_, ok := parseWithFlexibleResult(raw, "text")
		if ok {
			t.Errorf("expected hollow rejection for %q, got ok=true", raw)
		}
	}
}

// TestParseWithFlexibleResult_InvalidJSONReturnsFalse verifies that
// invalid JSON is rejected.
func TestParseWithFlexibleResult_InvalidJSONReturnsFalse(t *testing.T) {
	_, ok := parseWithFlexibleResult(`not json at all`, "text")
	if ok {
		t.Error("expected false for invalid JSON, got true")
	}
}

// TestParseOutput_StringResultRoundTrip verifies the parsed output can be
// re-marshaled to valid JSON — important for downstream consumers.
func TestParseOutput_StringResultRoundTrip(t *testing.T) {
	raw := `{"status":"ok","confidence":0.9,"result":"Hello!"}`
	out := ParseOutput(raw, "text")

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Re-unmarshal to verify it's valid JSON with the expected shape.
	var back SubagentOutput
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Status != StatusOK {
		t.Errorf("round-trip status: want ok, got %s", back.Status)
	}
}
