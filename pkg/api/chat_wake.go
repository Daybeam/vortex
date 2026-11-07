package api

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Push actionable task states to the agent.
//
// Three callback subscriptions already existed, but only the terminal one woke
// the model: EventTaskCompleted/Failed called startFollowUpTurn, while
// EventDecisionRequired, the heartbeat events and the step/round progress
// events only did session.AddEvent — which appends to the SSE outbound queue.
// The model's input is built from history + tool results, so an event is not a
// message: the frontend saw "decision_needed" and the agent never did, leaving
// a blocked task with nobody to ask.
//
// What gets pushed is deliberately narrow. A wake costs a turn (2–3 provider
// requests) out of the same budget the sub-agent draws from, so only states
// that cannot resolve themselves qualify: a task waiting on a decision, and a
// terminal failure. Heartbeats and progress stay UI-only.
const (
	// wakeMaxPerTask caps how many non-terminal wakes one task may cause. A
	// task with five blocked steps must not turn a chat session into a log
	// stream; past the cap the event is already in the session for the user to
	// find, and the next user message will surface it.
	wakeMaxPerTask = 3

	// wakeMinInterval is the per-task floor between wakes. Decision gates can
	// fire in bursts (gate + executor for the same decision), and a model that
	// reacts to the first one needs a moment before it can be useful again.
	wakeMinInterval = 30 * time.Second
)

// wakeState is the per-task budget for wakes.
type wakeState struct {
	mu    sync.Mutex
	last  time.Time
	total int
}

// decisionWakeText renders the transient user message for a blocked task.
//
// EventDecisionRequired carries two shapes. The decision gate sends
// {decision_id, type, options}; the max-turns path sends
// {reason, error, retry_count, resume_hint} and no options at all. A renderer
// that only knows the first shape produces an empty instruction for the second,
// which is the more common way a step blocks.
func decisionWakeText(taskID, stepID string, payload map[string]any) string {
	options := stringSlice(payload["options"])
	kind := firstNonEmpty(
		str(payload["type"]),
		str(payload["reason"]),
		"needs_input",
	)
	var b strings.Builder
	fmt.Fprintf(&b, "[System notice] Delegated task %s is blocked and needs the user's decision.\n", taskID)
	if stepID != "" {
		fmt.Fprintf(&b, "Step: %s\n", stepID)
	}
	fmt.Fprintf(&b, "Reason: %s\n", kind)
	if e := str(payload["error"]); e != "" {
		fmt.Fprintf(&b, "Error: %s\n", truncateRunes(e, 300))
	}
	if hint := str(payload["resume_hint"]); hint != "" {
		fmt.Fprintf(&b, "Resume hint: %s\n", truncateRunes(hint, 300))
	}
	if len(options) > 0 {
		fmt.Fprintf(&b, "Options: %s\n", strings.Join(options, ", "))
	}
	b.WriteString("Ask the user which option to take, quoting the options verbatim. ")
	b.WriteString("Do not choose for them, and do not answer the original request with a guess.")
	return b.String()
}

// pendingWakeEntry holds a wake that could not start because a turn was in
// flight. finishTurn reads the queue after the current turn ends and starts a
// follow-up.
//
// reason is the value already normalised for startFollowUpTurnWithTrigger (it
// is "task_done" for a finished task and "wake:<cause>" for a pushed state), so
// the dedupe key built from it matches the one the starter would have used.
type pendingWakeEntry struct {
	taskID string
	reason string
	text   string
	// budgeted marks the entries that must pass the per-task wake budget
	// (wakeMaxPerTask / wakeMinInterval). Only pushed state — a blocked task's
	// decision notice — is budgeted; a finished task's result is not, because it
	// is a one-shot deliverable and the budget exists to stop log streams, not
	// to discard results. This is keyed off the caller rather than the reason
	// string on purpose: reading a name is how a new producer ends up silently
	// classified into the wrong policy (chat_write_guard's execute_code blind
	// spot is the same mistake in another file).
	budgeted bool
}

// maxPendingWakes caps the per-session FIFO. The cap exists so a pathological
// producer cannot grow it without bound; it is deliberately far above the
// realistic fan-out (one entry per finished task plus at most wakeMaxPerTask
// decision notices per task). Hitting it is logged, because every dropped entry
// is data the model will never see.
const maxPendingWakes = 8

// wakeQueue is the FIFO of deferred wakes for one session.
//
// It used to be a single slot ("the newest wake replaces the first"), which was
// defensible while decisions were the only producer — the newest decision is
// the most actionable. It is not defensible for task completions: a session
// that finishes two delegated tasks would silently lose the first result, and
// that is the same class of defect as dropping the wake outright.
type wakeQueue struct {
	mu      sync.Mutex
	entries []pendingWakeEntry
}

// push appends unless an equivalent wake is already queued. Returns false when
// the entry was a duplicate or the queue is full.
func (q *wakeQueue) push(e pendingWakeEntry) (duplicate bool, queued bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, cur := range q.entries {
		if cur.taskID == e.taskID && cur.reason == e.reason {
			return true, false
		}
	}
	if len(q.entries) >= maxPendingWakes {
		return false, false
	}
	q.entries = append(q.entries, e)
	return false, true
}

// pushFront puts an entry back at the head: used when delivery lost the race
// for the session and the wake must keep its position.
func (q *wakeQueue) pushFront(e pendingWakeEntry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.entries = append([]pendingWakeEntry{e}, q.entries...)
}

func (q *wakeQueue) popFront() (pendingWakeEntry, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.entries) == 0 {
		return pendingWakeEntry{}, false
	}
	e := q.entries[0]
	q.entries = q.entries[1:]
	return e, true
}

func (q *wakeQueue) empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries) == 0
}

// snapshot copies the queue for assertions in tests.
func (q *wakeQueue) snapshot() []pendingWakeEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]pendingWakeEntry, len(q.entries))
	copy(out, q.entries)
	return out
}

// enqueueWake defers one wake for a busy session. Returns true when it was
// queued (or was already queued) — false only when the queue refused it.
func (h *ChatHandler) enqueueWake(sessionID string, e pendingWakeEntry) bool {
	if h == nil || sessionID == "" || e.taskID == "" || e.text == "" {
		return false
	}
	v, _ := h.pendingWake.LoadOrStore(sessionID, &wakeQueue{})
	q, ok := v.(*wakeQueue)
	if !ok {
		return false
	}
	duplicate, queued := q.push(e)
	if duplicate || queued {
		return true
	}
	fmt.Fprintf(os.Stderr, "[chat-wake] session=%s task=%s queue full (%d); wake dropped\n",
		sessionID, e.taskID, maxPendingWakes)
	return false
}

// noteTaskConsumed records that the running turn was shown this task's terminal
// state. Cleared when the session's next turn claims the slot, so a marker can
// only ever suppress a wake for the turn that wrote it.
func (h *ChatHandler) noteTaskConsumed(sessionID, taskID string) {
	if h == nil || sessionID == "" || taskID == "" {
		return
	}
	h.consumedTasks.Store(sessionID+"|"+taskID, struct{}{})
}

// takeTaskConsumed consumes the marker: one delivery attempt, one suppression.
func (h *ChatHandler) takeTaskConsumed(sessionID, taskID string) bool {
	if h == nil {
		return false
	}
	_, ok := h.consumedTasks.LoadAndDelete(sessionID + "|" + taskID)
	return ok
}

// clearConsumedTasks drops any leftover markers for a session. Called when a
// turn claims the slot: markers describe "the turn that just ended", so they
// must not survive into the next one.
//
// The key separator is "|", so a session id that is a prefix of another cannot
// match — session ids never contain it.
func (h *ChatHandler) clearConsumedTasks(sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	prefix := sessionID + "|"
	h.consumedTasks.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok && strings.HasPrefix(s, prefix) {
			h.consumedTasks.Delete(k)
		}
		return true
	})
}

// wakeMain delivers one actionable state to the session's agent.
//
// It starts a follow-up turn, which is the only mechanism that puts text in
// front of the model when nobody is driving the session. Two floors apply
// before that: the dedupe key (the same decision must not be announced twice)
// and the per-task budget (see wakeMaxPerTask / wakeMinInterval).
//
// When the session is busy (a turn is in flight), the wake is queued in
// pendingWake instead of being dropped. finishTurn delivers it after the
// current turn ends — the model sees the decision without waiting for the
// next user message. Wakes are delivered oldest-first and none are discarded:
// the single-slot "newest replaces the first" rule it used to have also applied
// to finished-task results, where discarding the older entry loses a
// deliverable outright.
//
// Returns true when a turn was started or the wake was queued for delivery.
func (h *ChatHandler) wakeMain(sessionID, taskID, reason, text, dedupeKey string) bool {
	if h == nil || h.Harness == nil || sessionID == "" || taskID == "" || text == "" {
		return false
	}
	if dedupeKey != "" {
		if _, dup := h.wakeSeen.LoadOrStore(sessionID+"|"+taskID+"|"+dedupeKey, struct{}{}); dup {
			return false
		}
	}
	if !h.reserveWake(taskID) {
		return false
	}
	switch h.startFollowUpTurnWithTrigger(sessionID, taskID, "wake:"+reason, text) {
	case followUpStarted:
		return true
	case followUpBusy:
		// Session is busy — queue the wake for finishTurn to deliver after
		// the current turn ends. The reservation is released because the
		// budget should not count against a later attempt; the queued wake
		// re-reserves when finishTurn delivers it.
		h.releaseWake(taskID)
		return h.enqueueWake(sessionID, pendingWakeEntry{
			taskID:   taskID,
			reason:   "wake:" + reason,
			text:     text,
			budgeted: true,
		})
	default:
		// A dead cause (duplicate / depth cap): queueing it would retry a wake
		// that can never start, but the budget it reserved is not spent.
		h.releaseWake(taskID)
		return false
	}
}

func (h *ChatHandler) wakeStateFor(taskID string) *wakeState {
	if v, ok := h.wakeStates.Load(taskID); ok {
		if st, ok := v.(*wakeState); ok {
			return st
		}
	}
	st := &wakeState{}
	actual, _ := h.wakeStates.LoadOrStore(taskID, st)
	if cur, ok := actual.(*wakeState); ok {
		return cur
	}
	return st
}

func (h *ChatHandler) reserveWake(taskID string) bool {
	st := h.wakeStateFor(taskID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.total >= wakeMaxPerTask {
		return false
	}
	if !st.last.IsZero() && time.Since(st.last) < wakeMinInterval {
		return false
	}
	st.last = time.Now()
	st.total++
	return true
}

func (h *ChatHandler) releaseWake(taskID string) {
	st := h.wakeStateFor(taskID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.total > 0 {
		st.total--
	}
	st.last = time.Time{}
}

// cleanupTaskChatState removes all chat-handler bookkeeping entries keyed by
// taskID. Called when a task reaches a terminal state (completed/failed) so
// the dedup/wake-state maps don't grow unbounded across the process lifetime.
//
// audit H2/H3/H4 (2026-11-05): followUpSeen, wakeSeen, and wakeStates had
// Store/LoadOrStore but no Delete anywhere — every follow-up and wake event
// created a permanent entry, leaking memory on a long-running server. This
// reclaims them at the natural eviction point (task terminal). progressLast
// is already cleaned at the call site.
func (h *ChatHandler) cleanupTaskChatState(taskID string) {
	if taskID == "" {
		return
	}
	h.wakeStates.Delete(taskID)
	needle := "|" + taskID + "|"
	h.wakeSeen.Range(func(k, _ any) bool {
		if strings.Contains(k.(string), needle) {
			h.wakeSeen.Delete(k)
		}
		return true
	})
	h.followUpSeen.Range(func(k, _ any) bool {
		if strings.Contains(k.(string), needle) {
			h.followUpSeen.Delete(k)
		}
		return true
	})
}

// cleanupSessionChatState removes all chat-handler bookkeeping entries scoped
// to sessionID. Called when a session is cleared (/clear) so per-session dedup
// maps don't retain entries for evicted sessions. audit H2/H3/H4.
func (h *ChatHandler) cleanupSessionChatState(sessionID string) {
	if sessionID == "" {
		return
	}
	prefix := sessionID + "|"
	h.followUpSeen.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			h.followUpSeen.Delete(k)
		}
		return true
	})
	h.wakeSeen.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			h.wakeSeen.Delete(k)
		}
		return true
	})
}

// deliverPendingWake starts a follow-up turn for the oldest wake that was
// deferred while the session was busy. Called by finishTurn after the current
// turn ends.
//
// At most one turn is started per call: the chain continues by itself, because
// that turn's finishTurn drains the next entry. Entries that cannot start a turn
// are consumed rather than retried forever:
//
//   - the turn that just ended already showed the model this task's terminal
//     state (consumed marker) — the cause was delivered, a second answer would
//     be a duplicate;
//   - a budgeted wake whose per-task budget is spent, or a cause that is a
//     duplicate of one already delivered, or a session past the depth cap.
//     A finished task's result is not budgeted: the budget exists to stop a
//     blocked task from streaming notices, not to discard deliverables.
//
// The queue object stays in the map for the session's lifetime, even when empty.
// Deleting it when it drains looks tidier and is wrong: "check empty, then
// CompareAndDelete" races an enqueue that lands in between, and the delete takes
// the just-queued entry with it — the same silent loss, now in a rarer window. A
// wakeQueue is a mutex and a nil slice, so keeping one per session costs nothing
// next to the chat session it belongs to.
func (h *ChatHandler) deliverPendingWake(sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	v, ok := h.pendingWake.Load(sessionID)
	if !ok {
		return
	}
	q, ok := v.(*wakeQueue)
	if !ok || q == nil {
		return
	}

	for {
		entry, ok := q.popFront()
		if !ok {
			return
		}
		// The turn that just ended already read this result. Delivering it
		// again would produce a second answer and a second turn for work that
		// was finished — the defect the old "claim fails ⇒ drop" rule was
		// written to avoid, now applied only when it is actually true.
		if h.takeTaskConsumed(sessionID, entry.taskID) {
			h.followUpSeen.Store(sessionID+"|"+entry.taskID+"|"+entry.reason, struct{}{})
			fmt.Fprintf(os.Stderr, "[chat-wake] session=%s task=%s wake dropped: "+
				"the turn that just ended already read this task's result\n", sessionID, entry.taskID)
			continue
		}
		if entry.budgeted && !h.reserveWake(entry.taskID) {
			continue // budget spent — the event is still in the session queue
		}
		switch h.startFollowUpTurnWithTrigger(sessionID, entry.taskID, entry.reason, entry.text) {
		case followUpStarted:
			return
		case followUpBusy:
			// Another turn claimed the slot between finishTurn freeing it and
			// this call (a user message, or a concurrent follow-up). Put the
			// entry back at the head: it must keep its place, not be dropped.
			q.pushFront(entry)
			if entry.budgeted {
				h.releaseWake(entry.taskID)
			}
			return
		default:
			if entry.budgeted {
				h.releaseWake(entry.taskID)
			}
		}
	}
}

// ── small helpers (kept local so the payload rendering stays pure) ──────────

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func stringSlice(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
