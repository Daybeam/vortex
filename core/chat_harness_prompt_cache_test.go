package core

import (
	"strings"
	"testing"
)

// The system prompt is the first bytes of every request, so it has to be
// byte-identical between turns or the provider's prefix cache is worthless.
//
// It used to take (query, taskID) and inject an archive search keyed on the
// latest user message. Every new user turn therefore produced a different
// system prompt and the cached prefix never matched — measured on the
// harness-bench traces, cached_input_tokens was 4–13% of input_tokens.
func TestBuildSystem_IsIndependentOfTheTurn(t *testing.T) {
	h := &ChatHarness{}

	a := h.buildSystem()
	b := h.buildSystem()
	if a != b {
		t.Fatal("system prompt changed between two calls on the same harness")
	}
	if strings.Contains(a, "Relevant prior context") {
		t.Error("per-turn retrieval must not live in the system prompt; see buildTurnContext")
	}
}

func TestBuildTurnContext_NoArchiveIsEmpty(t *testing.T) {
	h := &ChatHarness{}
	if got := h.buildTurnContext("anything", "task"); got != "" {
		t.Errorf("expected empty context without an archive, got %q", got)
	}
}
