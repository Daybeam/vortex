package core

import (
	"strings"

	"github.com/daybeam/vortex/store"
)

// malformedToolCallTags are the pseudo-XML tags a model emits when it tries to
// call a tool but does not use the platform's native JSON tool-call structure.
// They are matched against the raw response text so the spawn loop can inject a
// correction prompt instead of silently accepting the text as a final answer.
//
// <anim:call> and <tool_call> were the original two. <tool_code> was missing:
// Qwen3-9B emits "<tool_code> app__tool </tool_code>" on AppWorld (v20,
// 2026-10-22), which the guard did not recognise, so ParseOutput downgraded the
// turn to partial and the step was accepted with zero effect. See eval
// FINDINGS.md §A1.
var malformedToolCallTags = []string{"<anim:call", "<tool_call", "<tool_code"}

// hasMalformedToolCallTag reports whether text contains a pseudo-XML tool-call
// tag. A true result means the model expressed a tool call in an invalid format
// and should be re-prompted to use the native JSON structure.
func hasMalformedToolCallTag(text string) bool {
	for _, tag := range malformedToolCallTags {
		if strings.Contains(text, tag) {
			return true
		}
	}
	return false
}

// traceHasEffect reports whether any call in the trace could have changed
// external state. Shared predicate of the two zero-work completion gates:
// the zero-effect gate (declared ok / parse-collapsed with a read-only trace)
// and the mid-abandonment gate (A5, parseable partial with a read-only
// trace). Kept next to the malformed-tag guard so every "is the model
// actually doing anything" check lives in one file.
func traceHasEffect(trace []store.ToolInteraction) bool {
	for _, t := range trace {
		if !IsReadOnlyToolName(t.ToolName) {
			return true
		}
	}
	return false
}
