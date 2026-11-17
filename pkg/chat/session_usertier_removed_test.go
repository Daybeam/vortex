package chat

import (
	"reflect"
	"testing"
)

// O36 (2026-11-06, Q3-1 answer: "suggest deleting the dead O36 field while you're at it"): ChatSession.UserTier
// was written by nobody and read by nobody. The tier that actually gates a chat
// turn lives on pkg/api.ChatRequest and is resolved from the caller, never from
// the session. The compiler proves the deletion; this tripwire stops the field
// from drifting back in through a future "the session needs a tier" change that
// forgets tiers are decided per request (register §O36).

func TestChatSessionUserTierStaysDeleted(t *testing.T) {
	if _, ok := reflect.TypeOf(ChatSession{}).FieldByName("UserTier"); ok {
		t.Fatal("ChatSession.UserTier is back; no code writes it and per-request tiers live " +
			"on pkg/api.ChatRequest (register §O36)")
	}
}
