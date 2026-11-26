package api

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
)

// Follow-up turns — the missing fourth segment of async delegation.
//
// The delegation chain was ① submit → ② the sub-agent runs → ③ task_callback
// lands in the session. Segment ④ — turning that result into an answer — did
// not exist: WireTaskCompletionCallbacks only did AddEvent + AppendMessage +
// persist, and the only caller of harness.Run in the whole repository is
// SendHandler, i.e. "the user said something". So a delegated task that
// outlived its turn had its result written into the session tree and never
// read, which is exactly what happened to harness-bench 026 on v58: the turn
// died at the 300 s budget, the task kept running, completed, and the round
// produced nothing.
//
// The trigger condition is causal, not heuristic: start a follow-up turn iff
// nobody is currently driving this session. Concretely —
//
//   - task finishes while the turn is still in flight
//     ⇒ the wake is QUEUED, not dropped. finishTurn delivers it, and whether it
//     is delivered depends on one signal: did that turn actually show the model
//     this task's terminal state (ChatHarness.OnTerminalTaskSeen)? If yes —
//     harness-bench 026/v60 (167 s, delegated, read, answered) — the queued
//     wake is suppressed: one answer, no duplicate. If no — harness-bench p24 —
//     the turn ended without reading the result, so the queued wake runs and
//     delivers it.
//     The rule used to be "claim failed ⇒ drop", which assumed the first case
//     unconditionally. p24 is the second case: the result was written into the
//     session tree, never read, and the only trace was one log line.
//   - task finishes after the turn ended (or died)
//     ⇒ the claim succeeds, one follow-up turn runs and answers.
//
// The claim is the same h.cancelFuncs slot SendHandler uses, taken with
// LoadOrStore rather than LoadAndDelete. That ordering matters: SendHandler
// *cancels* whatever it finds in that slot (:1002), so a follow-up that naively
// stored its own cancel func could be killed by the next user message — and,
// worse, could kill a user turn by storing into a live slot. LoadOrStore makes
// both impossible: whoever holds the slot keeps it.
const (
	// maxFollowUpDepth caps chained follow-ups per session. A follow-up turn
	// can delegate again, whose completion starts another follow-up; without a
	// cap, two models that keep handing work back and forth would run forever.
	maxFollowUpDepth = 3

	// followUpTriggerFmt is the transient user message that starts the turn.
	// It is deliberately NOT written to the session tree: the user should see
	// one extra answer, not a synthetic "user" message they never typed.
	//
	// "Do not rewrite the artifacts" is a design rule, not a measured fix. It
	// was added after p9-followup-forced (026) saw this turn read the sub-agent's
	// review_findings.json, re-derive it, and write the file back, with the
	// score at 0.15 versus 0.7 for the same task on the previous image. p10 then
	// produced 0.15 again WITHOUT any rewrite (the turn was still alive, so no
	// follow-up ran at all), and the two 0.15 runs fail the identical oracle
	// cases — so the score difference was model variance in the review prose,
	// not the rewrite. The rule stands on its own grounds: the follow-up exists
	// to deliver a result, and a second attempt at the work is a different
	// feature with a different cost.
	followUpTriggerFmt = "[System notice] Delegated task %s has finished; its output is in the workspace. " +
		"Read the artifacts it produced and answer the user's original request now. " +
		"Do not re-run the task from scratch, and do not rewrite the artifacts it produced — " +
		"they are the deliverable. Only create a file if the task produced none."
)

// followUpOutcome is what happened to a wake request.
//
// Only followUpBusy may be queued. A suppressed cause — an already-delivered
// one, a session past the depth cap, a session that no longer exists — must be
// dropped, or the queue turns a chain of follow-ups into an unbounded loop over
// causes that can never start.
type followUpOutcome int

const (
	// followUpStarted means a turn was launched and now owns the session.
	followUpStarted followUpOutcome = iota
	// followUpBusy means a turn (or another follow-up) holds the session. The
	// caller should queue the wake; dropping it loses the result silently.
	followUpBusy
	// followUpSuppressed means the cause is dead: duplicate, depth cap, or no
	// session/harness. Queueing it would retry forever.
	followUpSuppressed
)

// startFollowUpTurn resumes a session after a delegated task finished.
//
// Returns true only when a turn actually started. A busy session is not a
// failure: the wake is queued and finishTurn delivers it, so the result is not
// lost (harness-bench p24 lost exactly this way — one line in the log and no
// answer). See enqueueWake and deliverPendingWake.
func (h *ChatHandler) startFollowUpTurn(sessionID, taskID, summary string) bool {
	return h.startFollowUpTurnWithSeed(sessionID, taskID, summary, wakeSeed{})
}

// wakeSeed is everything a wake turn inherits from the finished task besides
// its summary. Grouped rather than passed as three more parameters because the
// chain already takes five, and because two adjacent strings of the same type
// is how a call site transposes digest and fallback without failing to build.
//
// Each field is optional and independent: the digest changes what the model
// sees, the fallback what the user gets when the model never runs, the
// declaration what the turn can observe about its own deliverable. They are
// gated by three separate switches for that reason — measuring them together
// makes each one's effect unattributable.
type wakeSeed struct {
	digest   string
	fallback string
	// declared is the finished task's completion contract (taskOutcome.Declared).
	// It is carried on the wake instead of being re-parsed from the trigger: the
	// trigger names no artifacts, and the task outcome is the only thing that
	// knows them (FINDINGS §6.3d).
	declared []string
	// figures is the canonical headline-figure token list, computed at enqueue
	// time from the declared artifacts. Carried for the same reason as fallback
	// and declared: delivery happens later, in another goroutine, and by then
	// the task outcome that knows the artifact contents is gone.
	figures []string
	// contracts keeps field types and optionality intact through the same deferred hop.
	contracts []schemas.ArtifactContract
}

func normalizeWakeSeed(seed wakeSeed) wakeSeed {
	if len(seed.contracts) == 0 && len(seed.declared) > 0 {
		seed.contracts = make([]schemas.ArtifactContract, 0, len(seed.declared))
		for _, path := range seed.declared {
			seed.contracts = append(seed.contracts, schemas.ArtifactContract{Path: path})
		}
	}
	seed.contracts = schemas.CloneArtifactContracts(seed.contracts)
	return seed
}

// startFollowUpTurnWithDigest is the form that also carries the artifact digest.
//
// It exists as a separate method rather than a fourth parameter because the digest
// is produced where the task outcome is known (chat_outbox.go) and the rest of the
// call sites — including the whole follow-up test file — have nothing to add. The
// gate is inside wakeArtifactDigest, so passing "" and not calling it are the same
// behaviour.
//
// fallback is what the session gets if this turn cannot answer at all — see
// wake_fallback.go. It is built by the caller because the caller is the one
// holding the task outcome, and it is only ever appended on the failure path, so
// a run whose wake turn succeeds is byte-for-byte unchanged.
func (h *ChatHandler) startFollowUpTurnWithDigest(sessionID, taskID, summary, digest, fallback string) bool {
	return h.startFollowUpTurnWithSeed(sessionID, taskID, summary,
		wakeSeed{digest: digest, fallback: fallback})
}

// startFollowUpTurnWithSeed is the form that carries all three channels.
func (h *ChatHandler) startFollowUpTurnWithSeed(sessionID, taskID, summary string, seed wakeSeed) bool {
	seed = normalizeWakeSeed(seed)
	reason := "task_done"
	text := fmt.Sprintf(followUpTriggerFmt, taskID)
	// The trigger says "read the artifacts it produced" without naming them, so
	// the turn has to find them by guessing. Measured on 2026-11-06 (task_75f448f3):
	// the task had produced none, the turn was told nothing about that, and it
	// answered the user's request from its own priors instead of reporting the
	// gap. The notification text already carries status, confidence and the file
	// list, so hand it over rather than letting the parameter go unused.
	if summary != "" {
		text += "\n\n" + summary
	}
	// The digest is the content half of the same problem (D21, 2026-11-17): a file
	// list tells the model what exists, not what it says, and a wake turn that is
	// handed only names answered with invented figures in 10/10 runs on 041.
	if seed.digest != "" {
		text += seed.digest
	}
	switch h.startFollowUpTurnWithTriggerSeed(sessionID, taskID, reason, text, seed) {
	case followUpStarted:
		return true
	case followUpBusy:
		h.enqueueWake(sessionID, pendingWakeEntry{
			taskID: taskID, reason: reason, text: text, digest: seed.digest,
			fallback: seed.fallback, declared: seed.declared,
			figures: seed.figures, contracts: seed.contracts,
		})
		return false
	default:
		return false
	}
}

// startFollowUpTurnWithTrigger is the general form: it starts a turn with an
// arbitrary transient user message and dedupes on (session, task, reason).
//
// reason exists because a session can now be woken for more than one cause —
// a finished task, a task blocked on a decision — and the same task must be
// allowed one wake per cause without the terminal path swallowing the other.
func (h *ChatHandler) startFollowUpTurnWithTrigger(sessionID, taskID, reason, trigger, fallback string) followUpOutcome {
	return h.startFollowUpTurnWithTriggerSeed(sessionID, taskID, reason, trigger,
		wakeSeed{fallback: fallback})
}

// prepareWakeHarness clones the session's harness for a wake turn and seeds it
// with the wake contract, delegated tasks, and the delegate/terminal-task-seen
// callbacks. Extracted from startFollowUpTurnWithTriggerSeed for size ratchet
// compliance.
func (h *ChatHandler) prepareWakeHarness(sessionID string, seed wakeSeed) *chat.ChatHarness {
	hCopy := cloneHarnessForRequest(h.harnessForWake(sessionID))
	if mode := wakeContractMode(); mode != chat.WakeContractOff {
		hCopy.SeedWakeContract(seed.contracts, mode)
	}
	hCopy.SeedDelegatedTasks(h.delegatedTasksFor(sessionID))
	isAdmin, _ := h.lastSubmitterAdmin.Load(sessionID)
	admin, _ := isAdmin.(bool)
	hCopy.Delegate = func(prompt string, maxTurnsCap int) (string, error) {
		id, err := h.submitChatTask(
			[]schemas.StepInput{{ID: "s1", Task: prompt, SubmitterIsAdmin: admin, MaxTurnsCap: maxTurnsCap}},
			sessionID)
		if err == nil {
			h.noteDelegatedTask(sessionID, id)
		}
		return id, err
	}
	hCopy.OnTerminalTaskSeen = func(taskID string) {
		h.noteTaskConsumed(sessionID, taskID)
	}
	hCopy.OnFileRead = func(basename string) {
		h.noteFileRead(sessionID, basename)
	}
	return hCopy
}

func (h *ChatHandler) startFollowUpTurnWithTriggerSeed(sessionID, taskID, reason, trigger string, seed wakeSeed) followUpOutcome {
	seed = normalizeWakeSeed(seed)
	// The failure path below is the only reader; aliased once so the delivery
	// code keeps reading as it did before the other two channels existed.
	fallback := seed.fallback
	if h == nil || h.Harness == nil || sessionID == "" || taskID == "" {
		return followUpSuppressed
	}
	session := h.Store.Get(sessionID)
	if session == nil {
		return followUpSuppressed
	}

	// One follow-up per (session, task, reason). A task can publish
	// EventTaskCompleted and a second terminal event; both would otherwise
	// start a turn and the user would get the same answer twice.
	//
	// Checked here, recorded only after the slot is claimed: a wake that loses
	// the claim (a turn is in flight) delivered nothing, and marking it as
	// delivered would silently drop that cause forever.
	seenKey := sessionID + "|" + taskID + "|" + reason
	if _, dup := h.followUpSeen.Load(seenKey); dup {
		return followUpSuppressed
	}

	// Cron-prefixed reasons (scheduled task delivery) bypass the session-level
	// follow-up depth cap: per-job quota is enforced by Schedule.MaxRuns, not
	// maxFollowUpDepth. Without this, a schedule running every 10 minutes would
	// exhaust the session's depth cap (3) in 30 minutes and become un-wakeable.
	depth := 0
	if v, ok := h.followUpDepth.Load(sessionID); ok {
		depth, _ = v.(int)
	}
	isCronWake := strings.HasPrefix(reason, "cron:")
	if !isCronWake && depth >= maxFollowUpDepth {
		fmt.Fprintf(os.Stderr, "[chat-followup] session=%s task=%s skipped: follow-up depth %d reached the cap (%d)\n",
			sessionID, taskID, depth, maxFollowUpDepth)
		return followUpSuppressed
	}

	runCtx, cancel := context.WithTimeout(context.Background(), chatTurnTimeout())

	// Atomic claim: nobody else may be driving this session. See the header
	// comment for why this is LoadOrStore and not Load+Store.
	if _, busy := h.cancelFuncs.LoadOrStore(sessionID, context.CancelFunc(cancel)); busy {
		cancel()
		fmt.Fprintf(os.Stderr, "[chat-followup] session=%s task=%s deferred: a turn is already in flight\n", sessionID, taskID)
		return followUpBusy
	}
	h.followUpSeen.Store(seenKey, struct{}{})
	if !isCronWake {
		h.followUpDepth.Store(sessionID, depth+1)
	}
	// New turn: markers left by the previous one must not suppress its wakes.
	h.clearConsumedTasks(sessionID)
	h.clearReadFiles(sessionID)

	// Announce the turn before it starts. Two consumers depend on it: an SSE
	// client that reconnected after `done` (it can then keep reading instead of
	// guessing from silence whether a turn is coming), and a human reading the
	// transcript, for whom "the assistant answered twice" is otherwise a
	// mystery. Absence of the event is the contract for "no turn was started".
	session.AddEvent(chat.ChatEvent{
		Type: "followup_turn",
		Data: "resuming the session (" + reason + ") for task " + taskID,
		Meta: map[string]any{"task_id": taskID, "turn": depth + 1},
	})

	h.wg.Add(1)

	go func() {
		terminalSent := false
		// Held back while a fallback is armed, forwarded if nothing replaces it.
		var heldError *chat.ChatEvent
		defer h.wg.Done()
		// The closure is load-bearing: arguments to a deferred call are evaluated
		// when the defer is REGISTERED, so `defer h.finishTurn(session,
		// terminalSent, recover())` would pass false and nil — a synthetic
		// "ended without a terminal event" error on every turn, and no panic
		// recovery at all.
		defer func() {
			h.finishTurn(session, terminalSent, recover())
		}()
		// Runs before finishTurn (defers are LIFO) so a held error still reaches
		// the reader when no report took its place.
		defer func() {
			if heldError != nil && !terminalSent {
				session.AddEvent(*heldError)
				terminalSent = true
			}
		}()
		defer cancel()
		defer h.cancelFuncs.Delete(sessionID)

		// Clone the harness the session's last request actually ran on, not the
		// shared one: a role-bound request builds its own harness and leaves
		// `h.Harness` roleless, so cloning the shared one made every wake turn
		// resume on the default identity and the default model — measured in
		// 8/8 runs of both arms before harnessForWake existed. See
		// chat_session_harness.go.
		hCopy := h.prepareWakeHarness(sessionID, seed)

		// A distinct task id: the Sieve result cache and the artifact directory
		// are keyed by it, and reusing the original turn's id would make the
		// follow-up read turn-1 cached reads as if they were its own.
		turnTaskID := "chat_" + sessionID + "_fu" + strconv.Itoa(depth+1)
		history := append(session.SnapshotMessages(), chat.ChatMessage{
			Role:    "user",
			Content: trigger,
		})

		fmt.Fprintf(os.Stderr, "[chat-followup] session=%s task=%s starting follow-up turn %s (depth %d)\n",
			sessionID, taskID, turnTaskID, depth+1)

		// A panic inside Run is covered by the deferred finishTurn above: it
		// recovers, logs the stack, and emits a terminal event, so the SSE
		// stream closes instead of hanging until the client's own deadline.
		final, err := hCopy.Run(runCtx, turnTaskID, history, func(ev chat.ChatEvent) error {
			if ev.Type == "error" && fallback != "" {
				// A terminal event ends the SSE stream and the reader stops at the
				// first one, so forwarding this error would leave the report in the
				// session tree and nowhere the waiting user can see it. Measured on
				// v93 (s93fb-on-r4): the 1886-character report appeared once in
				// chat/<session>.json and zero times in events.sse, while the stream
				// ended on `error`. Holding it here is what makes the fallback a
				// delivery instead of a database write.
				held := ev
				heldError = &held
				return nil
			}
			if ev.Type == "done" || ev.Type == "error" {
				terminalSent = true
			}
			session.AddEvent(ev)
			return nil
		})
		if err != nil && !chat.IsPartialRun(err) {
			fmt.Fprintf(os.Stderr, "[chat-followup] session=%s task=%s turn failed: %v\n", sessionID, taskID, err)
			// Silence here is how 48 of 117 delegating runs in the archive
			// delivered nothing while averaging 0.680 with 15 perfect scores
			// (wake_fallback.go, FINDINGS 2026-11-17 §1.9.1).
			h.deliverWakeFallback(session, fallback, wakeFallbackReasonBudget, &terminalSent)
			return
		}
		if final != "" {
			session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: final}, "")
			h.persistSession(session)
			return
		}
		// A turn that ran and produced no text is the same loss as one that never
		// ran: the artifacts are there, the user has nothing to read.
		h.deliverWakeFallback(session, fallback, wakeFallbackReasonNoText, &terminalSent)
	}()
	return followUpStarted
}
