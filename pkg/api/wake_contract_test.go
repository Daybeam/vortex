package api

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/daybeam/vortex/pkg/chat"
)

// The gate has three states and no fourth. A typo must land on OFF: this is the
// variable a batch script sets, and "unrecognised" silently meaning "treat the
// wake turn to a completion contract" would turn a measurement arm into a
// treatment arm without anyone editing code.
func TestWakeContractMode_Gate(t *testing.T) {
	tests := []struct {
		raw  string
		want chat.WakeContractMode
	}{
		{"", chat.WakeContractOff},
		{"off", chat.WakeContractOff},
		{"0", chat.WakeContractOff},
		{"false", chat.WakeContractOff},
		{"observe", chat.WakeContractObserve},
		{" OBSERVE ", chat.WakeContractObserve},
		{"on", chat.WakeContractFull},
		{"1", chat.WakeContractFull},
		{"true", chat.WakeContractFull},
		{"full", chat.WakeContractFull},
		{"observ", chat.WakeContractOff},  // typo: not a prefix match
		{"enabled", chat.WakeContractOff}, // plausible, but not a value this gate accepts
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(wakeContractEnv, tc.raw)
			if got := wakeContractMode(); got != tc.want {
				t.Fatalf("VORTEX_WAKE_CONTRACT=%q → %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// The seam that no unit test in pkg/chat can see: the declaration has to survive
// the trip from the finished task's outcome, through the wake turn's own
// harness, into Run. Asserting on the run's own summary line is the only place
// that proves it — every step in between is a plain field assignment whose
// failure mode is "still 0", not a compile error.
func TestWakeContract_ReachesTheWakeTurn(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "capacity_plan.json", `{"total_hourly_cost_usd":0.552}`)

	tests := []struct {
		gate     string
		declared string // the substring the summary line must carry
		why      string
	}{
		{"observe", "contract_declared=1 contract_steer=0", "observe must put the declaration on the summary line"},
		{"", "contract_declared=0", "gate off must leave the wake turn exactly as it was"},
	}
	for _, tc := range tests {
		t.Run("gate="+tc.gate, func(t *testing.T) {
			t.Setenv(wakeContractEnv, tc.gate)
			h, store, _ := newFollowUpHandler(t, "The plan is in capacity_plan.json.")
			h.Harness.SessionRoot = root
			store.GetOrCreate("s_wc")

			var logs bytes.Buffer
			prevOut := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(prevOut)

			if !h.startFollowUpTurnWithSeed("s_wc", "task_wc", "task finished",
				wakeSeed{declared: []string{"capacity_plan.json"}}) {
				t.Fatal("follow-up turn was not started for an idle session")
			}
			h.wg.Wait()

			// The wake turn's own task id, not the front turn's: Run is keyed by
			// chat_<session>_fu1 and the front turn never ran here.
			line := ""
			for _, cand := range strings.Split(logs.String(), "\n") {
				if strings.Contains(cand, "[chat-turn-summary] task=chat_s_wc_fu1") {
					line = cand
				}
			}
			if line == "" {
				t.Fatalf("no summary line for the wake turn; logs:\n%s", logs.String())
			}
			if !strings.Contains(line, tc.declared) {
				t.Fatalf("%s\n summary: %s", tc.why, line)
			}
		})
	}
}

// Observe must not get as far as the completion block: with an artifact that is
// absent, the armed path would append a defect steer to a tool result, and that
// is the treatment §6.3d says has to be measured separately.
func TestWakeContract_ObserveDoesNotArmChecks(t *testing.T) {
	t.Setenv(wakeContractEnv, "observe")
	root := t.TempDir() // capacity_plan.json deliberately absent
	h, store, _ := newFollowUpHandler(t, "nothing to report")
	h.Harness.SessionRoot = root
	store.GetOrCreate("s_wc_bare")

	var logs bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prevOut)

	if !h.startFollowUpTurnWithSeed("s_wc_bare", "task_wc", "task finished",
		wakeSeed{declared: []string{"capacity_plan.json"}}) {
		t.Fatal("follow-up turn was not started for an idle session")
	}
	h.wg.Wait()

	line := ""
	for _, cand := range strings.Split(logs.String(), "\n") {
		if strings.Contains(cand, "[chat-turn-summary] task=chat_s_wc_bare_fu1") {
			line = cand
		}
	}
	if line == "" {
		t.Fatalf("no summary line for the wake turn; logs:\n%s", logs.String())
	}
	// The observation reports the truth: the artifact is missing.
	if !strings.Contains(line, "contract_present=0 contract_missing=1") {
		t.Fatalf("the missing artifact was not reported; summary: %s", line)
	}
	// And the steer did not fire, which is the whole difference between the two
	// modes at this seam. Both counters sit on the same line as the observation,
	// so a run that armed would show a non-zero one next to missing=1.
	if !strings.Contains(line, "contract_steer=0") || !strings.Contains(line, "contract_defect_steer=0") {
		t.Fatalf("observe armed a steer; summary: %s", line)
	}
}

// The property that makes the observe arm readable at all: it changes one log
// field and nothing the provider can see. Compared at the provider boundary —
// the system and user text of the wake turn's request — because that is where a
// "harmless" instrumentation change would show up if it were not harmless.
//
// If this test ever fails, the 2×2 in §6.3d has a third variable in it and the
// observe arm is no longer a control.
func TestWakeContract_ObserveChangesNothingTheModelSees(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "capacity_plan.json", `{"total_hourly_cost_usd":0.552}`)

	run := func(gate string) string {
		t.Setenv(wakeContractEnv, gate)
		h, store, provider := newFollowUpHandler(t, "The plan is in capacity_plan.json.")
		h.Harness.SessionRoot = root
		store.GetOrCreate("s_wc_same")
		if !h.startFollowUpTurnWithSeed("s_wc_same", "task_wc", "task finished",
			wakeSeed{declared: []string{"capacity_plan.json"}}) {
			t.Fatalf("gate=%q: follow-up turn was not started", gate)
		}
		h.wg.Wait()
		if provider.calls == 0 {
			t.Fatalf("gate=%q: the wake turn never reached the provider", gate)
		}
		return provider.lastIn
	}

	off := run("")
	observe := run("observe")
	if off != observe {
		t.Fatalf("observe changed the request the model sees:\n--- off ---\n%s\n--- observe ---\n%s", off, observe)
	}
}

// A wake deferred because the session is busy is delivered later, by another
// goroutine, after the task outcome that knows the artifact names is gone. The
// queue carries them for the same reason it carries the fallback.
func TestWakeContract_QueueKeepsTheDeclaration(t *testing.T) {
	h, store, _ := newFollowUpHandler(t, "answer")
	store.GetOrCreate("s_wc_busy")
	h.cancelFuncs.Store("s_wc_busy", context.CancelFunc(func() {}))

	declared := []string{"capacity_plan.json", "capacity_plan.md"}
	if h.startFollowUpTurnWithSeed("s_wc_busy", "task_wc", "finished",
		wakeSeed{fallback: "fallback body", declared: declared}) {
		t.Fatal("a busy session must not start a second turn")
	}

	v, ok := h.pendingWake.Load("s_wc_busy")
	if !ok {
		t.Fatal("the wake was dropped instead of queued")
	}
	q, ok := v.(*wakeQueue)
	if !ok {
		t.Fatalf("pendingWake holds %T, want *wakeQueue", v)
	}
	entries := q.snapshot()
	if len(entries) != 1 {
		t.Fatalf("queued %d entries, want 1", len(entries))
	}
	if got := entries[0].declared; len(got) != 2 || got[0] != declared[0] || got[1] != declared[1] {
		t.Fatalf("the queued wake lost the declaration: got %v, want %v", got, declared)
	}
	if entries[0].fallback != "fallback body" {
		t.Fatalf("the existing fallback channel broke: %q", entries[0].fallback)
	}
}

// The drain seam: a queued wake is delivered by finishTurn, not by the call that
// created it, and the delivery path is a different function. The declaration has
// to survive that hop too, which is the one link neither of the tests above
// covers — the queue test stops at the entry, the reach test starts at the seed.
func TestWakeContract_SurvivesTheQueueDrain(t *testing.T) {
	t.Setenv(wakeContractEnv, "observe")
	root := t.TempDir()
	writeFile(t, root, "capacity_plan.json", `{"total_hourly_cost_usd":0.552}`)
	h, store, _ := newFollowUpHandler(t, "The plan is in capacity_plan.json.")
	h.Harness.SessionRoot = root
	store.GetOrCreate("s_wc_drain")

	// Enqueue exactly as a busy session would, then free the slot and drain.
	h.cancelFuncs.Store("s_wc_drain", context.CancelFunc(func() {}))
	if h.startFollowUpTurnWithSeed("s_wc_drain", "task_wc", "finished",
		wakeSeed{declared: []string{"capacity_plan.json"}}) {
		t.Fatal("a busy session must not start a turn")
	}
	h.cancelFuncs.Delete("s_wc_drain")

	var logs bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prevOut)

	h.deliverPendingWake("s_wc_drain")
	h.wg.Wait()

	line := ""
	for _, cand := range strings.Split(logs.String(), "\n") {
		if strings.Contains(cand, "[chat-turn-summary] task=chat_s_wc_drain_fu1") {
			line = cand
		}
	}
	if line == "" {
		t.Fatalf("the queued wake never became a turn; logs:\n%s", logs.String())
	}
	if !strings.Contains(line, "contract_declared=1") {
		t.Fatalf("the drain dropped the declaration; summary: %s", line)
	}
	if !strings.Contains(line, "contract_present=1") {
		t.Fatalf("the drained turn did not observe its artifact; summary: %s", line)
	}
}

// A decision wake has no outcome and therefore no declaration. It must not
// acquire one, or the blocked-task turn would be told about artifacts nobody
// produced.
func TestWakeContract_DecisionWakeHasNoDeclaration(t *testing.T) {
	h, store, _ := newFollowUpHandler(t, "which option?")
	store.GetOrCreate("s_wc_dec")
	h.cancelFuncs.Store("s_wc_dec", context.CancelFunc(func() {}))

	// true means "started or queued" — the busy session makes it the second.
	if !h.wakeMain("s_wc_dec", "task_wc", "decision", "pick one", "dec:1") {
		t.Fatal("the decision wake was neither started nor queued")
	}
	v, _ := h.pendingWake.Load("s_wc_dec")
	q, _ := v.(*wakeQueue)
	entries := q.snapshot()
	if len(entries) != 1 {
		t.Fatalf("queued %d entries, want 1", len(entries))
	}
	if len(entries[0].declared) != 0 {
		t.Fatalf("a decision wake acquired a contract: %v", entries[0].declared)
	}
}
