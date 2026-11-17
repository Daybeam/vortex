package api

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/daybeam/vortex/pkg/chat"
)

// Host-side watchdog for delegated tasks that overrun their estimate.
//
// Why the check belongs here and not in the model
// ----------------------------------------------
// "If it is late, check once what it is doing" is a reasonable instruction, but
// issuing it as a TOOL CALL costs a model turn, and model turns are the scarce
// resource: a wake is 2-3 provider requests out of the same budget the
// sub-agent draws from, wakeMaxPerTask caps a task at 3 pushed states, and
// maxFollowUpDepth caps a whole session at 3 follow-up turns. Spending one of
// those to ask "are you still alive" is exactly the trade the poll lever exists
// to remove (§9.12).
//
// So the check is a status READ, which this process can do for free, and the
// model is woken only when the read finds something it could act on: no
// progress across a full extra estimate. Two consequences that are the point of
// the whole file:
//
//   - the wake carries a diagnosis (which step, how long stuck), not "still
//     running" — the latter is what makes a caller poll again;
//   - a task that is merely slow never wakes the model at all, because the
//     watchdog re-arms instead (see delegWatch.next).
//
// What this does NOT do
// ---------------------
// It does not cancel, retry or auto-fail. `core/stale_task_monitor.go` owns the
// last of those and operates on a different scale (hourly tick, 24 h timeout) —
// that is a safety net for a process that died, not a progress watchdog, and
// reusing it here was rejected: its default threshold is ~1000x the duration of
// the tasks this is for.
//
// Default off
// -----------
// A watchdog that fires during a benchmark run changes the arm. Like
// VORTEX_POST_DELEG_POLL, this is opt-in so it can be measured against the same
// image (D14: a score delta with two candidate causes is not a measurement).

const (
	// watchSweepInterval is how often the ledger is scanned. The scan is an
	// in-memory Range plus one status read per due task, so it is cheap; the
	// interval only sets how late a check can be.
	watchSweepInterval = 10 * time.Second

	// watchBlindDelay is used when the estimate is unknown (no plan yet). It is
	// deliberately long: a task with no plan has nothing to extrapolate from, so
	// checking early can only produce "no news", and the first check must not
	// burn the one wake this watchdog is allowed.
	watchBlindDelay = 3 * time.Minute

	// watchFirstLook is when the very first status read happens. Its only job is
	// to turn "no estimate" into a real one, so it must land after the engine has
	// written a plan but before the task's own expected duration.
	watchFirstLook = 20 * time.Second

	// watchMaxPerTask caps how many times one task may wake the model for a
	// stall. One: the notice says the real wake still arrives, so a second
	// notice adds nothing the model can use and eats the shared budget.
	watchMaxPerTask = 1

	// watchMaxChecks caps free status reads per task, so a task whose progress
	// signature keeps changing cannot keep the sweep alive forever.
	watchMaxChecks = 12
)

// delegWatch is the per-task watchdog state, stored as the value in
// h.delegatedTasks. The key stays "sessionID|taskID" so the poll lever and the
// watchdog drain one ledger, not two that can disagree about who delegated what.
type delegWatch struct {
	// progress is the last signature seen. Equality across a full extra
	// estimate is the whole definition of "stuck" used here — the same signal
	// core/stale_task_monitor.go uses (a hash that has not changed), scaled down
	// from 24 h to the task's own expected duration.
	progress string
	next     time.Time
	checks   int
	woken    int
}

// delegWatchdogMode reports whether the watchdog is armed.
// VORTEX_DELEG_WATCHDOG=on|1|true enables; anything else is off.
func delegWatchdogMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VORTEX_DELEG_WATCHDOG"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// watchOnce starts the sweep the first time this handler delegates a task.
//
// Started lazily rather than in NewChatHandler so that (a) a deployment that
// never delegates runs no goroutine, and (b) it cannot observe the handler
// before its DelegateStatus is wired — the template harness is assigned to
// h.Harness during construction, and a goroutine started there would race that
// assignment in exactly the window where a missed wake is invisible.
func (h *ChatHandler) watchOnce() {
	if h == nil || !delegWatchdogMode() {
		return
	}
	h.delegWatchOnce.Do(func() {
		stop := h.delegWatchStop
		if stop == nil {
			return // constructed without a channel: nothing can be stopped later
		}
		if h.Harness == nil || h.Harness.DelegateStatus == nil {
			// Say so rather than sweeping into a nil reader and reporting
			// nothing forever.
			fmt.Fprintf(os.Stderr, "[deleg-watchdog] enabled but DelegateStatus is nil; watchdog idle\n")
			return
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			t := time.NewTicker(watchSweepInterval)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					h.sweepDelegWatch()
				}
			}
		}()
		fmt.Fprintf(os.Stderr, "[deleg-watchdog] armed (sweep %s, blind delay %s, max wakes/task %d)\n",
			watchSweepInterval, watchBlindDelay, watchMaxPerTask)
	})
}

// sweepDelegWatch reads every watched task's status and acts on the ones whose
// deadline has passed.
func (h *ChatHandler) sweepDelegWatch() {
	if h == nil || h.Harness == nil || h.Harness.DelegateStatus == nil {
		return
	}
	now := time.Now()
	type action struct {
		key      string
		session  string
		task     string
		deleteMe bool
		wake     string
	}
	var acts []action

	h.delegatedTasks.Range(func(k, v any) bool {
		key, ok := k.(string)
		if !ok {
			return true
		}
		w, ok := v.(*delegWatch)
		if !ok || w == nil {
			return true // a bare struct{} left by an older writer: not watched
		}
		if now.Before(w.next) {
			return true
		}
		session, task := splitDelegKey(key)
		if session == "" || task == "" {
			return true
		}
		st, found := h.Harness.DelegateStatus(task, 0)
		if !found || st == nil {
			// The task is gone from this engine (never started, or the ledger
			// outlived it). Nothing to watch and nothing to wake for: the
			// terminal path owns delivery, and an unknown task has no result.
			acts = append(acts, action{key: key, deleteMe: true})
			return true
		}
		if s, _ := st["status"].(string); isTerminalStatus(s) {
			acts = append(acts, action{key: key, deleteMe: true})
			return true
		}

		sig := progressSignature(st)
		if sig != w.progress {
			// Progress since the last look. Re-arm from the NEW estimate: a task
			// that is halfway through legitimately takes longer than one that
			// just started, and holding it to the original deadline is how a
			// watchdog turns into a nag.
			w.progress = sig
			w.checks++
			if w.checks >= watchMaxChecks {
				acts = append(acts, action{key: key, deleteMe: true})
				return true
			}
			w.next = now.Add(watchDelay(st, now))
			return true
		}

		// No progress across a full estimate. This is the only branch that
		// spends budget, and it spends it once.
		if w.woken >= watchMaxPerTask {
			acts = append(acts, action{key: key, deleteMe: true})
			return true
		}
		w.woken++
		acts = append(acts, action{key: key, session: session, task: task,
			wake: stuckWakeText(task, st, now, w)})
		return true
	})

	for _, a := range acts {
		if a.deleteMe {
			h.delegatedTasks.Delete(a.key)
			continue
		}
		if !h.wakeMain(a.session, a.task, "watchdog", a.wake, "watchdog") {
			// Not delivered: depth cap, duplicate, queue full, or no live
			// session. Logged because a dropped wake is invisible everywhere
			// else — the deliverable is never read and the run just ends with no
			// answer, which reads like a model failure (iron law 230).
			fmt.Fprintf(os.Stderr, "[deleg-watchdog] session=%s task=%s stuck notice NOT delivered\n",
				a.session, a.task)
			continue
		}
		fmt.Fprintf(os.Stderr, "[deleg-watchdog] session=%s task=%s stuck notice delivered\n",
			a.session, a.task)
	}
}

// watchDelay is how long to wait before the next look: the estimate's own
// watchdog delay, or the blind floor when there is no estimate to trust.
func watchDelay(st map[string]any, now time.Time) time.Duration {
	if d := chat.EstimateETA(st, now).WatchdogDelay(); d > 0 {
		return d
	}
	return watchBlindDelay
}

// splitDelegKey undoes the "sessionID|taskID" join. Session ids never contain
// "|" (see noteTaskConsumed), so the last separator is unambiguous.
func splitDelegKey(key string) (session, task string) {
	i := strings.LastIndex(key, "|")
	if i <= 0 || i == len(key)-1 {
		return "", ""
	}
	return key[:i], key[i+1:]
}

// progressSignature reduces a status dict to "has anything changed".
//
// Step statuses and the presence of an error are included; timings are not,
// because elapsed time always changes and a signature that always changes
// reports every task as healthy forever.
func progressSignature(st map[string]any) string {
	steps, _ := st["steps"].(map[string]any)
	ids := make([]string, 0, len(steps))
	for id := range steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	if s, _ := st["status"].(string); s != "" {
		b.WriteString("graph=" + s + ";")
	}
	for _, id := range ids {
		sm, ok := steps[id].(map[string]any)
		if !ok {
			continue
		}
		s, _ := sm["status"].(string)
		b.WriteString(id + "=" + s)
		if e, _ := sm["last_error"].(string); e != "" {
			b.WriteString("/err")
		}
		b.WriteString(";")
	}
	return b.String()
}

// stuckWakeText is the notice the model sees. It has to answer three questions
// in one turn, because there is no second turn: what is stuck, how stuck, and
// what it is allowed to do about it. Telling it to "take over" would duplicate
// the work the task is still doing (followUpTriggerFmt's "do not re-run the
// task" rule applies here too).
func stuckWakeText(taskID string, st map[string]any, now time.Time, w *delegWatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[System notice] Delegated task %s has not made progress for longer than expected.\n", taskID)
	if steps, ok := st["steps"].(map[string]any); ok && len(steps) > 0 {
		done, total := 0, len(steps)
		var stuckStep string
		for _, sv := range steps {
			sm, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			s, _ := sm["status"].(string)
			switch s {
			case "ok", "partial", "skipped", "failed":
				done++
			case "running", "blocked":
				if id, _ := sm["id"].(string); id != "" {
					stuckStep = id + " (" + s + ")"
				}
			}
		}
		fmt.Fprintf(&b, "Progress: %d/%d steps settled", done, total)
		if stuckStep != "" {
			b.WriteString(", currently on " + stuckStep)
		}
		b.WriteString(".\n")
	}
	if created, _ := st["created_at"].(string); created != "" {
		if t, err := time.Parse(time.RFC3339, created); err == nil {
			fmt.Fprintf(&b, "Elapsed: %s.\n", formatWatchDuration(now.Sub(t)))
		}
	}
	b.WriteString("This is a notice, not a failure: the task is still running and you will still be woken when it " +
		"finishes. Do not start the work yourself and do not rewrite what it has already written.\n")
	b.WriteString("Tell the user which step it is on and how long it has been there, then stop. " +
		"If they want it stopped, that is their call, not a retry.\n")
	return b.String()
}

// isTerminalStatus mirrors chat's terminal set for the coarse string that
// statusData carries. Kept local because pkg/api must not reach into chat's
// unexported helpers, and the two lists are short enough to compare by eye if
// the engine ever adds a terminal state.
func isTerminalStatus(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "completed", "completed_with_skips", "failed", "cancelled", "canceled":
		return true
	}
	return false
}

// formatWatchDuration is the compact form used in the notice. Separate from
// chat's formatDuration because that one is unexported and rounding to whole
// minutes here would report a 70 s stall as "1m", which reads as normal.
func formatWatchDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// noteDelegWatch arms the watchdog for one delegation. Called from
// noteDelegatedTask so the ledger entry and its watch state are written in one
// place and cannot disagree about whether a task is watched.
//
// The first look is early and its only job is to replace "no estimate" with a
// real one: at delegation time the task has no plan yet, so arming straight to
// a future deadline would mean the first real measurement happens after the
// point where it would have been useful.
func (h *ChatHandler) noteDelegWatch(key string, now time.Time) {
	if h == nil || key == "" || !delegWatchdogMode() {
		return
	}
	h.delegatedTasks.Store(key, &delegWatch{progress: "", next: now.Add(watchFirstLook)})
}
