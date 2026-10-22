package core

import "testing"

// Regression for A1 (eval FINDINGS.md §A1, 2026-10-22):
// The malformed-tool-call guard only matched <anim:call and <tool_call, so
// Qwen3-9B's `<tool_code> app__tool </tool_code>` pseudo-call on AppWorld (v20)
// slipped through untouched. ParseOutput then downgraded the turn to partial
// and the step was accepted with zero effect — the model had the right intent
// but the engine never recognised the format.
//
// Before the fix: hasMalformedToolCallTag did not exist and the inline guard
// missed <tool_code, so the "tool_code" case below would not be intercepted.
// After the fix: every pseudo-XML tool-call tag is recognised.
func TestHasMalformedToolCallTag(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"anim_call", `<anim:call> {"name":"x"}</anim:call>`, true},
		{"tool_call", `calling <tool_call>foo</tool_call>`, true},
		// The exact shape observed in v20 (task 6c2c621_1 → task_103ed01c).
		{"tool_code (A1)", "<tool_code> | simple_note__search_notes | </tool_code>", true},
		{"plain_json", `{"name":"search","arguments":{}}`, false},
		{"prose", "I have completed the task.", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasMalformedToolCallTag(tc.text); got != tc.want {
				t.Errorf("hasMalformedToolCallTag(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}