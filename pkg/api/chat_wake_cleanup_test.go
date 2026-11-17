package api

import (
	"testing"
)

// TestCleanupTaskChatState_ReclaimsMapEntries is the regression test for
// audit H2/H3/H4 (2026-11-05): followUpSeen, wakeSeen, and wakeStates had
// Store/LoadOrStore but no Delete, so every follow-up and wake event leaked a
// permanent entry. cleanupTaskChatState (called on task terminal) must remove
// every entry keyed by that taskID while leaving other tasks' entries intact.
func TestCleanupTaskChatState_ReclaimsMapEntries(t *testing.T) {
	h := &ChatHandler{}
	// Populate as the production code does.
	h.followUpSeen.Store("sessA|taskX|done", struct{}{})
	h.followUpSeen.Store("sessA|taskY|done", struct{}{}) // other task, must survive
	h.wakeSeen.Store("sessA|taskX|dec", struct{}{})
	h.wakeSeen.Store("sessB|taskX|fail", struct{}{})
	h.wakeSeen.Store("sessB|taskZ|dec", struct{}{}) // other task, must survive
	h.wakeStates.Store("taskX", &wakeState{})
	h.wakeStates.Store("taskY", &wakeState{})

	h.cleanupTaskChatState("taskX")

	if _, ok := h.wakeStates.Load("taskX"); ok {
		t.Fatal("wakeStates[taskX] should be deleted on task terminal")
	}
	if _, ok := h.wakeStates.Load("taskY"); !ok {
		t.Fatal("wakeStates[taskY] (other task) must survive")
	}
	// D7: followUpSeen must SURVIVE a per-task cleanup. notifyTaskTerminal
	// claims the (session, task, reason) slot and then calls this function at
	// its end, so reclaiming the entry here deleted the record the same call had
	// just written -- a redelivered terminal event then started another
	// follow-up turn (nine of them in one measured run). The entry belongs to
	// the session, not to the task, and cleanupSessionChatState is what reclaims
	// it; see TestFollowUpTurn_SecondTerminalEventStartsNoSecondTurn.
	if _, ok := h.followUpSeen.Load("sessA|taskX|done"); !ok {
		t.Fatal("followUpSeen entry for taskX must survive cleanupTaskChatState (D7)")
	}
	if _, ok := h.followUpSeen.Load("sessA|taskY|done"); !ok {
		t.Fatal("followUpSeen entry for taskY (other task) must survive")
	}
	h.cleanupSessionChatState("sessA")
	if _, ok := h.followUpSeen.Load("sessA|taskX|done"); ok {
		t.Fatal("cleanupSessionChatState must reclaim the session's followUpSeen entries")
	}
	if _, ok := h.wakeSeen.Load("sessA|taskX|dec"); ok {
		t.Fatal("wakeSeen entry sessA|taskX should be deleted")
	}
	if _, ok := h.wakeSeen.Load("sessB|taskX|fail"); ok {
		t.Fatal("wakeSeen entry sessB|taskX should be deleted")
	}
	if _, ok := h.wakeSeen.Load("sessB|taskZ|dec"); !ok {
		t.Fatal("wakeSeen entry for taskZ (other task) must survive")
	}
}

// TestCleanupSessionChatState_ReclaimsMapEntries is the session-level
// counterpart: /clear must evict every per-session dedup entry so cleared
// sessions don't retain followUpSeen/wakeSeen entries forever (audit H2/H3/H4).
func TestCleanupSessionChatState_ReclaimsMapEntries(t *testing.T) {
	h := &ChatHandler{}
	h.followUpSeen.Store("sessA|taskX|done", struct{}{})
	h.followUpSeen.Store("sessB|taskX|done", struct{}{}) // other session, must survive
	h.wakeSeen.Store("sessA|taskX|dec", struct{}{})
	h.wakeSeen.Store("sessA|taskY|fail", struct{}{})
	h.wakeSeen.Store("sessB|taskX|dec", struct{}{})

	h.cleanupSessionChatState("sessA")

	if _, ok := h.followUpSeen.Load("sessA|taskX|done"); ok {
		t.Fatal("followUpSeen entry for sessA should be deleted on /clear")
	}
	if _, ok := h.followUpSeen.Load("sessB|taskX|done"); !ok {
		t.Fatal("followUpSeen entry for sessB (other session) must survive")
	}
	if _, ok := h.wakeSeen.Load("sessA|taskX|dec"); ok {
		t.Fatal("wakeSeen entry for sessA should be deleted on /clear")
	}
	if _, ok := h.wakeSeen.Load("sessA|taskY|fail"); ok {
		t.Fatal("wakeSeen entry for sessA should be deleted on /clear")
	}
	if _, ok := h.wakeSeen.Load("sessB|taskX|dec"); !ok {
		t.Fatal("wakeSeen entry for sessB (other session) must survive")
	}
}

// TestCleanupTaskChatState_EmptyTaskID_NoOp ensures the helper is a safe no-op
// for an empty taskID (defensive — event payloads can be empty).
func TestCleanupTaskChatState_EmptyTaskID_NoOp(t *testing.T) {
	h := &ChatHandler{}
	h.wakeStates.Store("taskX", &wakeState{})
	h.cleanupTaskChatState("") // must not panic and must not delete unrelated entries
	if _, ok := h.wakeStates.Load("taskX"); !ok {
		t.Fatal("empty taskID must be a no-op; taskX should survive")
	}
}

// /clear must reset the follow-up depth along with the dedupe maps.
//
// The counter is monotonic by design (it exists to stop two models handing work
// back and forth), but it was scoped to the process instead of the conversation:
// cleanupSessionChatState dropped followUpSeen and left followUpDepth, so after
// three wakes a cleared session could never be woken again — every new cause
// looked fresh, passed the dedupe, and was then suppressed at the cap with only
// a stderr line. Clearing must return the session to the state a fresh one has.
func TestCleanupSessionChatState_ResetsFollowUpDepth(t *testing.T) {
	h := &ChatHandler{}
	h.followUpDepth.Store("sessA", maxFollowUpDepth)
	h.followUpDepth.Store("sessB", 1)
	h.followUpSeen.Store("sessA|taskX|task_done", struct{}{})

	h.cleanupSessionChatState("sessA")

	if v, ok := h.followUpDepth.Load("sessA"); ok {
		t.Fatalf("cleared session kept follow-up depth %v; it can never be woken again", v)
	}
	if v, ok := h.followUpDepth.Load("sessB"); !ok || v.(int) != 1 {
		t.Fatalf("cleanup of sessA touched sessB's depth: %v %v", v, ok)
	}
	if _, ok := h.followUpSeen.Load("sessA|taskX|task_done"); ok {
		t.Fatal("followUpSeen was not reclaimed for the cleared session")
	}
}
