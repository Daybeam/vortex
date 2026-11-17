package api

import (
	"github.com/daybeam/vortex/pkg/chat"
)

// sessionHarness remembers the harness a session's last request actually ran
// on, so a wake turn resumes as the same agent.
//
// Why this exists (2026-11-11, measured on harness-bench 030): when a request
// resolves a role, the request path does NOT mutate the shared harness — it
// builds a NEW ChatHarness with that role's provider, model and Instruction
// (see "Role selection" in chat_handlers.go). The follow-up path cloned
// `h.Harness`, which never carries a role, so every `_fuN` wake turn ran on the
// default identity:
//
//	main turn request : "# Role: Chat Front Desk ..."
//	wake turn request : "You are a concise, helpful assistant in a standalone
//	                     chat harness."
//
// That was true in 8 of 8 runs of both arms, so it is not a race — it is the
// default path. It was invisible until the polling lever started moving work
// INTO the wake turn: off vs wake scored .830 vs .705 with one extra zero,
// because in the wake arm the turn that finishes the task is the one that lost
// its role (and its role's model).
//
// Storing the harness rather than a role id keeps provider/model resolution
// exactly as the request resolved it — re-resolving would duplicate the
// front-desk fallback rules and could pick a different model than the one that
// answered the user. cloneHarnessForRequest still makes a fresh copy per turn,
// so this pointer is a template, never driven directly.
func (h *ChatHandler) noteSessionHarness(sessionID string, hh *chat.ChatHarness) {
	if sessionID == "" || hh == nil {
		return
	}
	h.sessionHarness.Store(sessionID, hh)
}

// sessionHarnessFor returns the harness template for a session, or nil when no
// request has run in it yet (then the caller keeps using the shared harness).
func (h *ChatHandler) sessionHarnessFor(sessionID string) *chat.ChatHarness {
	v, ok := h.sessionHarness.Load(sessionID)
	if !ok {
		return nil
	}
	hh, _ := v.(*chat.ChatHarness)
	return hh
}

// forgetSessionHarness drops the template. Called from
// cleanupSessionChatState with the rest of the per-session state; keeping it
// would pin a provider and a role for a session nobody is talking in.
func (h *ChatHandler) forgetSessionHarness(sessionID string) {
	h.sessionHarness.Delete(sessionID)
}

// harnessForWake returns the template a wake turn must clone. Split out from
// the goroutine so the fallback is testable: nil store → shared harness.
func (h *ChatHandler) harnessForWake(sessionID string) *chat.ChatHarness {
	if sh := h.sessionHarnessFor(sessionID); sh != nil {
		return sh
	}
	return h.Harness
}
