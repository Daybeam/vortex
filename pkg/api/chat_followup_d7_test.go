package api

import (
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
)

// TestFollowUpTurn_SecondTerminalEventStartsNoSecondTurn pins D7.
//
// A task can publish more than one terminal event; the outbox already dedupes
// the *queued notification* on `task_callback|<task>` (see
// TestNotifyTaskTerminal_SecondTerminalEventQueuesOnce). The resumed turn had no
// such protection: notifyTaskTerminal claims the (session, task, reason) slot,
// and then the same function calls cleanupTaskChatState, which deletes every
// followUpSeen key containing "|task|" -- i.e. it deletes the record it just
// wrote. A redelivery therefore finds an empty dedupe table and starts another
// turn.
//
// Observed in a real run (156-subtitle-timing-format-repair, gemini,
// --max-agent-turns 80): nine `[run] followup: engine started a turn
// (resuming the session (task_done) for task task_03f044bd)` lines for one
// task, and no result.json for eight minutes.
func TestFollowUpTurn_SecondTerminalEventStartsNoSecondTurn(t *testing.T) {
	hubA := newHub(t, filepath.Join(t.TempDir(), "hub.db"))
	provider := &followUpProvider{answer: "acknowledged"}
	hubA.h.Harness = &chat.ChatHarness{
		Provider:   provider,
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   4,
	}

	sess := hubA.h.Store.GetOrCreate("s_dup")
	hubA.register("task_dup", "s_dup", schemas.GraphCompleted)

	ev := obs.NewAgentEvent("task_dup", "", string(obs.EventTaskCompleted), nil)
	if err := hubA.h.notifyTaskTerminal(ev); err != nil {
		t.Fatalf("first terminal event: %v", err)
	}
	hubA.h.wg.Wait()

	if err := hubA.h.notifyTaskTerminal(ev); err != nil {
		t.Fatalf("second terminal event: %v", err)
	}
	hubA.h.wg.Wait()

	if n := countEvents(sess, "followup_turn"); n != 1 {
		t.Errorf("one task produced %d follow-up turns, want 1", n)
	}
	if provider.calls != 1 {
		t.Errorf("provider called %d time(s) for one finished task, want 1", provider.calls)
	}
}
