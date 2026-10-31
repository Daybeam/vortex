package core

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncatePromptForEvent_MultiByteUTF8 is a regression test for D7
// truncation (DELEGATION_HANDOVER_REDESIGN.md §10.2): the old code cut the
// marshaled JSON at 8192 BYTES, which could sever UTF-8 multi-byte characters
// (producing invalid UTF-8) or cut mid-JSON (producing unparseable fragments).
//
// This test constructs a prompt with multi-byte UTF-8 content that exceeds
// 8192 bytes when marshaled, then verifies:
//  1. The result is ≤ 8192 bytes
//  2. The result is valid JSON (unmarshalable)
//  3. The result is valid UTF-8 (no severed multi-byte sequences)
//
// Before the fix: truncated[:8192] could cut a 3-byte Chinese char at byte 2,
// producing invalid UTF-8 AND invalid JSON (unclosed string).
func TestTruncatePromptForEvent_MultiByteUTF8(t *testing.T) {
	// Build a prompt with multi-byte UTF-8 content (Chinese characters, 3 bytes
	// each in UTF-8). 3000 chars × 3 bytes = 9000 bytes > 8192.
	// Each '中' is 3 bytes in UTF-8.
	bigString := strings.Repeat("中", 3000)

	prompt := map[string]any{
		"system_prompt": "You are a helpful assistant.",
		"user_prompt":   bigString,
	}

	result := truncatePromptForEvent(prompt, 8192)

	// 1. Must fit within the byte limit.
	if len(result) > 8192 {
		t.Errorf("result length %d exceeds 8192 bytes", len(result))
	}

	// 2. Must be valid JSON.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("D7 regression: result is not valid JSON: %v\nresult[:200]=%q", err, result[:min(200, len(result))])
	}

	// 3. Must be valid UTF-8.
	if !utf8.ValidString(result) {
		t.Fatal("D7 regression: result contains invalid UTF-8 (severed multi-byte character)")
	}

	// 4. The system_prompt should survive (it's small).
	if sp, ok := decoded["system_prompt"].(string); !ok || sp != "You are a helpful assistant." {
		t.Errorf("expected system_prompt to survive truncation, got %v", decoded["system_prompt"])
	}

	// 5. The user_prompt should be truncated (not the full 3000 chars).
	if up, ok := decoded["user_prompt"].(string); ok {
		if len([]rune(up)) >= 3000 {
			t.Error("expected user_prompt to be truncated, but it contains all 3000 runes")
		}
	}
}

// TestTruncatePromptForEvent_SmallPromptNoTruncation verifies that prompts
// under the byte limit are returned as-is without any truncation.
func TestTruncatePromptForEvent_SmallPromptNoTruncation(t *testing.T) {
	prompt := map[string]any{
		"system_prompt": "You are a helper.",
		"user_prompt":   "What is 2+2?",
	}

	result := truncatePromptForEvent(prompt, 8192)

	var decoded map[string]any
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if up, ok := decoded["user_prompt"].(string); !ok || up != "What is 2+2?" {
		t.Errorf("expected user_prompt unchanged, got %v", decoded["user_prompt"])
	}
}

// TestTruncatePromptForEvent_OldByteCutWouldFail demonstrates that the old
// approach (cutting marshaled JSON at 8192 bytes) produces invalid JSON or
// invalid UTF-8 for multi-byte content. This test documents the defect the
// fix addresses — it does NOT test the fix itself.
func TestTruncatePromptForEvent_OldByteCutWouldFail(t *testing.T) {
	bigString := strings.Repeat("中", 3000)
	prompt := map[string]any{
		"user_prompt": bigString,
	}

	b, err := json.Marshal(prompt)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	if len(b) <= 8192 {
		t.Skip("prompt fits within 8192 bytes; cannot demonstrate the defect")
	}

	// Simulate the old byte-cut approach.
	oldCut := string(b[:8192])

	// The old cut either produces invalid UTF-8 or invalid JSON (or both).
	// At minimum, it produces invalid JSON because it cuts mid-string.
	oldJSONErr := json.Unmarshal([]byte(oldCut), &map[string]any{})
	oldUTF8Valid := utf8.ValidString(oldCut)

	if oldJSONErr == nil && oldUTF8Valid {
		t.Log("unexpected: old byte-cut produced valid JSON and UTF-8 (got lucky with alignment)")
	}
	// In practice, one or both will be invalid — that's the defect D7 fixes.
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
