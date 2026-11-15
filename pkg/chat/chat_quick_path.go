package chat

import "fmt"

// QuickPathMatch holds a verified crystallized skill match for the quick path.
// When SkillQuickPath returns a non-nil match, the harness steers the LLM to
// use the skill's JIT tool directly. See CRYSTALLIZED_SKILL_QUICKPATH_DESIGN.md §4.
type QuickPathMatch struct {
	ToolName  string
	SkillID   string
	Signature string
}

// applyQuickPath consults the SkillQuickPath hook and, if it returns a match,
// appends a directive to system that tells the LLM to use the matched skill's
// JIT tool directly. Returns the (possibly augmented) system prompt.
func (h *ChatHarness) applyQuickPath(query, system string, emit func(ChatEvent) error) string {
	if h.SkillQuickPath == nil || query == "" {
		return system
	}
	match := h.SkillQuickPath(query)
	if match == nil {
		return system
	}
	directive := fmt.Sprintf(
		"\n## VERIFIED SKILL QUICK PATH\n"+
			"A verified tool %q is available that directly handles this type of task.\n"+
			"Use the tool %q to complete the user's request. Extract the appropriate\n"+
			"parameters from the user's message and call the tool directly.\n",
		match.ToolName, match.ToolName)
	if match.Signature != "" {
		directive += "Tool signature: " + match.Signature + "\n"
	}
	emit(ChatEvent{Type: "quick_path", Data: match.ToolName, Meta: map[string]any{
		"skill_id": match.SkillID,
	}})
	return system + directive
}
