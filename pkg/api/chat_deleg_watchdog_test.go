package api

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/chat"
)

// A watchdog that wakes on its own schedule changes the arm it runs in, so the
// gate has to be proven off, not assumed off (iron law 220). This is the test
// that makes "v90 and v91 differ only by the flag" a claim rather than a hope.
func TestDelegWatchdog_DefaultsOff(t *testing.T) {
	t.Setenv("VORTEX_DELEG_WATCHDOG", "")
	if delegWatchdogMode() {
		t.Fatal("unset VORTEX_DELEG_WATCHDOG must mean off")
	}
	for _, v := range []string{"0", "off", "false", "no", "armed"} {
		t.Setenv("VORTEX_DELEG_WATCHDOG", v)
		if delegWatchdogMode() {
			t.Errorf("VORTEX_DELEG_WATCHDOG=%q must mean off", v)
		}
	}
	for _, v := range []string{"on", "1", "true", " ON "} {
		t.Setenv("VORTEX_DELEG_WATCHDOG", v)
		if !delegWatchdogMode() {
			t.Errorf("VORTEX_DELEG_WATCHDOG=%q must mean on", v)
		}
	}
}

// With the gate off, the ledger value must stay struct{}{} — the type the poll
// lever was written against and the only one that costs nothing to store.
func TestDelegWatchdog_OffStoresBareEntry(t *testing.T) {
	t.Setenv("VORTEX_DELEG_WATCHDOG", "")
	h := &ChatHandler{}
	h.noteDelegatedTask("sess-1", "task-1")
	v, ok := h.delegatedTasks.Load("sess-1|task-1")
	if !ok {
		t.Fatal("noteDelegatedTask did not record the delegation")
	}
	if _, isWatch := v.(*delegWatch); isWatch {
		t.Error("watchdog state was stored while the gate was off")
	}
	if ids := h.delegatedTasksFor("sess-1"); len(ids) != 1 || ids[0] != "task-1" {
		t.Errorf("poll-lever ledger broke: %v", ids)
	}
}

func TestSplitDelegKey(t *testing.T) {
	cases := map[string][2]string{
		"sess|task": {"sess", "task"},
		"sess|a|b":  {"sess|a", "b"}, // the last separator wins
		"nosep":     {"", ""},
		"|task":     {"", ""},
		"sess|":     {"", ""},
	}
	for in, want := range cases {
		s, task := splitDelegKey(in)
		if s != want[0] || task != want[1] {
			t.Errorf("splitDelegKey(%q) = %q,%q want %q,%q", in, s, task, want[0], want[1])
		}
	}
}

// The signature is the whole definition of "stuck", so it must be insensitive to
// everything that changes on its own (map order, elapsed time) and sensitive to
// the one thing that does not (a step's status).
func TestProgressSignature(t *testing.T) {
	a := map[string]any{"status": "running", "steps": map[string]any{
		"s1": map[string]any{"status": "ok"},
		"s2": map[string]any{"status": "running"},
	}}
	b := map[string]any{"status": "running", "steps": map[string]any{
		"s2": map[string]any{"status": "running"},
		"s1": map[string]any{"status": "ok"},
	}}
	if progressSignature(a) != progressSignature(b) {
		t.Error("signature depends on map iteration order")
	}
	c := map[string]any{"status": "running", "steps": map[string]any{
		"s1": map[string]any{"status": "ok"},
		"s2": map[string]any{"status": "ok"},
	}}
	if progressSignature(a) == progressSignature(c) {
		t.Error("signature missed the only change that matters: a step finished")
	}
}

func fakeStatus(st map[string]any) func(string, time.Duration) (map[string]any, bool) {
	return func(string, time.Duration) (map[string]any, bool) { return st, true }
}

// A terminal task is the callback path's business, not the watchdog's: the
// entry must be dropped so a finished task cannot be woken for being stuck.
func TestSweepDelegWatch_DropsTerminalTask(t *testing.T) {
	t.Setenv("VORTEX_DELEG_WATCHDOG", "on")
	h := &ChatHandler{Harness: &chat.ChatHarness{DelegateStatus: fakeStatus(map[string]any{
		"status": "completed",
		"steps":  map[string]any{"s1": map[string]any{"status": "ok"}},
	})}}
	h.delegatedTasks.Store("sess|task", &delegWatch{next: time.Now().Add(-time.Second)})
	h.sweepDelegWatch()
	if _, ok := h.delegatedTasks.Load("sess|task"); ok {
		t.Error("terminal task stayed on the watch list")
	}
}

// Progress re-arms from the new estimate instead of waking: a task that is
// merely slow must never cost a model turn.
func TestSweepDelegWatch_ProgressRearmsWithoutWaking(t *testing.T) {
	t.Setenv("VORTEX_DELEG_WATCHDOG", "on")
	created := time.Now().Add(-2 * time.Minute).Format(time.RFC3339)
	h := &ChatHandler{Harness: &chat.ChatHarness{DelegateStatus: fakeStatus(map[string]any{
		"status":     "running",
		"created_at": created,
		"steps": map[string]any{
			"s1": map[string]any{"status": "ok"},
			"s2": map[string]any{"status": "running"},
			"s3": map[string]any{"status": "pending"},
		},
	})}}
	w := &delegWatch{next: time.Now().Add(-time.Second)}
	h.delegatedTasks.Store("sess|task", w)
	h.sweepDelegWatch()

	got, ok := h.delegatedTasks.Load("sess|task")
	if !ok {
		t.Fatal("an unfinished task was dropped from the watch list")
	}
	cur := got.(*delegWatch)
	if cur.checks != 1 {
		t.Errorf("checks=%d, want 1", cur.checks)
	}
	if cur.woken != 0 {
		t.Errorf("woken=%d, want 0 — progress must not spend the wake", cur.woken)
	}
	if !cur.next.After(time.Now()) {
		t.Error("next look was not moved forward")
	}
}

// An unknown task (never started, or the ledger outlived the engine) has no
// result to deliver and nothing to be stuck on.
func TestSweepDelegWatch_DropsUnknownTask(t *testing.T) {
	t.Setenv("VORTEX_DELEG_WATCHDOG", "on")
	h := &ChatHandler{Harness: &chat.ChatHarness{DelegateStatus: func(string, time.Duration) (map[string]any, bool) {
		return nil, false
	}}}
	h.delegatedTasks.Store("sess|gone", &delegWatch{next: time.Now().Add(-time.Second)})
	h.sweepDelegWatch()
	if _, ok := h.delegatedTasks.Load("sess|gone"); ok {
		t.Error("unresolvable task stayed on the watch list")
	}
}
