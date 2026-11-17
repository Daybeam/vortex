package api

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/chat"
)

// A wake turn must resume as the agent that was talking to the user.
//
// The bug this pins down (2026-11-11, harness-bench 030): a role-bound request
// builds its own ChatHarness and never touches the shared one, so cloning
// `h.Harness` in the follow-up path handed every wake turn the default
// identity — "You are a concise, helpful assistant in a standalone chat
// harness." — and the default model. Present in 8 of 8 runs of both arms.
// Scores only moved once the polling lever made the wake turn do the work
// (.830 off vs .705 wake, one extra zero), which is why the missing role read
// as a lever defect for a while.
func TestWakeTurnInheritsTheRoleThatAnsweredTheUser(t *testing.T) {
	shared := &chat.ChatHarness{Model: "default-model"}
	h := &ChatHandler{Harness: shared}

	rolename := "Chat Front Desk"
	roleHarness := &chat.ChatHarness{
		Model: "role-model",
		Role:  &config.Role{Name: rolename, Instruction: "# Role: Chat Front Desk\n\nDo the thing."},
	}

	// Before any request: no template, fall back to the shared harness.
	if got := h.harnessForWake("s1"); got != shared {
		t.Fatalf("with no stored harness, want the shared one, got %p", got)
	}

	h.noteSessionHarness("s1", roleHarness)

	got := h.harnessForWake("s1")
	if got != roleHarness {
		t.Fatalf("wake turn ignored the session's role harness")
	}
	// The clone is what actually runs, so the role has to survive it.
	clone := cloneHarnessForRequest(got)
	if clone.Role == nil || clone.Role.Name != rolename {
		t.Fatalf("clone lost the role: %+v", clone.Role)
	}
	if clone.Model != "role-model" {
		t.Fatalf("clone lost the role's model: %q", clone.Model)
	}

	// Sessions are independent: a session that stored nothing keeps the default.
	if got := h.harnessForWake("s2"); got != shared {
		t.Fatalf("session s2 inherited s1's harness")
	}

	// Clearing the session must drop the template, not pin a provider forever.
	h.forgetSessionHarness("s1")
	if got := h.harnessForWake("s1"); got != shared {
		t.Fatalf("forgetSessionHarness left a stale template")
	}
}
