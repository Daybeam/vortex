package api

// Outbound chat notifications: what a finished or blocked delegated task tells
// the user, and how it gets there when this process is not the one the user is
// talking to.
//
// The three subscriptions here used to live inside WireTaskCompletionCallbacks
// and stop at AddEvent + AppendMessage. That reaches a client only while it is
// already watching: forwardEvents existed just for the duration of the user's
// own turn, and the session event ring is in-memory, so a task that outlived
// the turn, a decision that arrived after the turn's deadline, and anything
// produced by a hub that lost the getUpdates race all wrote into a buffer
// nobody read. Those notifications now also go on the durable queue
// (store.IChatOutbox), which a transport pumps out whenever it can reach the
// user -- see chat_telegram_outbox.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/store"
)

// Notification kinds are the same strings as the chat event types a live SSE
// listener sees, so a row read back after a restart rebuilds the event the
// listener would have gotten.
const (
	kindTaskCallback   = "task_callback"
	kindDecisionNeeded = "decision_needed"
)

// Structured-log sink for this pipeline. Naming the queue its own file keeps
// "why did I not get notified" answerable from one log: every notification is
// either queued, dropped with a reason, delivered, or failed with an error.
const chatOutboxSink = "chat_outbox"

// Event names for the outbox sink. None of them is an obs state-transition
// type: Logger.Log re-broadcasts anything with a non-empty second argument
// onto DefaultBus, and the pipeline must not feed the machine it observes.
// That argument carries the sink name here, as it does for the telegram sink.
const (
	eventNotificationQueued      = "EventNotificationQueued"
	eventNotificationDuplicate   = "EventNotificationDuplicate"
	eventNotificationDropped     = "EventNotificationDropped"
	eventNotificationFailed      = "EventNotificationFailed"
	eventNotificationUnconfirmed = "EventNotificationUnconfirmed"
	eventNotificationDowngraded  = "EventNotificationDowngraded"
	eventNotificationPurged      = "EventNotificationPurged"
)

// Reasons a notification has nowhere to go. Each one used to be an unlogged
// return, which is why a missing notification was indistinguishable from a
// task that never finished.
const (
	dropUnknownTask    = "task_not_in_this_hubs_scheduler"
	dropNoSessionBound = "task_has_no_session_id"
	dropSessionEvicted = "session_not_loaded_in_this_process"
)

// sessionRef is what the notification path needs to know about a task: the
// live session (nil when there is none), the status view it came from, the
// session id the task was bound to -- which survives the session itself being
// gone -- and, when there is no session, why.
type sessionRef struct {
	session    *chat.ChatSession
	statusData map[string]any
	sessionID  string
	dropReason string
}

// resolveSessionForTask looks the owning chat session up through the
// scheduler's status dict.
func (h *ChatHandler) resolveSessionForTask(taskID string) sessionRef {
	if taskID == "" {
		return sessionRef{dropReason: dropUnknownTask}
	}
	statusData, ok := h.App.Scheduler.GetStatus(taskID, "full")
	if !ok || statusData == nil {
		return sessionRef{dropReason: dropUnknownTask}
	}
	sessionID, _ := statusData["session_id"].(string)
	if sessionID == "" {
		return sessionRef{statusData: statusData, dropReason: dropNoSessionBound}
	}
	session := h.Store.Get(sessionID)
	if session == nil {
		return sessionRef{statusData: statusData, sessionID: sessionID, dropReason: dropSessionEvicted}
	}
	return sessionRef{session: session, statusData: statusData, sessionID: sessionID}
}

// notifyTaskTerminal records a finished delegated task: the structured event
// for a client that is watching, the message in the history tree for the next
// LLM turn, and a queue row for whichever hub can actually reach the user.
func (h *ChatHandler) notifyTaskTerminal(ev obs.AgentEvent) error {
	taskID := ev.TaskID
	ref := h.resolveSessionForTask(taskID)
	if ref.session == nil {
		// An unbound completion is logged, not pushed: nobody is waiting for a
		// task that was never attached to a conversation.
		h.logNotificationDrop(taskID, kindTaskCallback, ref.dropReason, ref.sessionID)
		return nil
	}
	session := ref.session
	statusData := ref.statusData

	taskStatus, _ := statusData["status"].(string)
	taskDesc, _ := statusData["task"].(string)
	if taskDesc == "" {
		taskDesc = taskID
	}

	// FIX (2026-10-27): the callback previously delivered only a text summary
	// ("task X completed") -- the frontend received no signal of WHAT was
	// produced. Pass through the output_files metadata and the final step's
	// result_ref that ToStatusDictView("full") already computes
	// (schemas/task.go:804), so the SSE listener can render the deliverable
	// (file list + primary summary) without a second round-trip to
	// /api/tasks/:id/steps/:stepId/result. On failure, surface last_error so
	// the user sees why, not just "failed".
	//
	// The same fields also go into the text, because that is all a pushed
	// notification can carry to a reader who is not watching.
	outcome := collectTaskOutcome(taskID, taskDesc, ev.EventType, statusData)
	summary := outcome.summary()

	meta := map[string]any{
		"task_id": taskID,
		"status":  taskStatus,
		"event":   ev.EventType,
	}
	if len(outcome.Files) > 0 {
		meta["output_files"] = outcome.Files
	}
	if outcome.ResultRef != "" {
		meta["result_ref"] = outcome.ResultRef
	}
	if outcome.LastError != "" {
		meta["last_error"] = outcome.LastError
	}

	session.AddEvent(chat.ChatEvent{Type: kindTaskCallback, Data: summary, Meta: meta})
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: summary}, "")
	h.persistSession(session)

	// Queued per session, not per kind: only a Telegram-shaped session has a
	// transport that can push it later. A web session replays from history.
	if isTelegramSession(ref.sessionID) {
		h.queueNotification(taskID, ref.sessionID, kindTaskCallback, "task_callback|"+taskID, summary, meta)
	}

	// Resume the session if this task outlived the turn that started it. The
	// claim inside decides: if a turn is in flight it keeps ownership (it can
	// already poll the task), otherwise exactly one follow-up turn runs and
	// turns the result into an answer. Without this segment a delegated task
	// whose turn died at its budget wrote its result into the session tree and
	// nobody ever read it.
	h.startFollowUpTurn(ref.sessionID, taskID, summary)

	// Stop tracking this task: the throttle entry is only needed while the task
	// is alive, and the map would otherwise grow for the process lifetime (one
	// entry per task ever run).
	h.progressLast.Delete(taskID)
	// audit H2/H3/H4: reclaim followUpSeen/wakeSeen/wakeStates entries for this
	// task so the dedup maps don't leak across the process lifetime.
	h.cleanupTaskChatState(taskID)
	return nil
}

// notifyHeartbeat forwards the spawner's VCP-style heartbeat events into the
// session so a waiting user sees what a delegated task is doing.
// See docs/plan/VCP_HEARTBEAT_AND_CONTEXT_TIERING_DESIGN.md §2.
func (h *ChatHandler) notifyHeartbeat(ev obs.AgentEvent) error {
	ref := h.resolveSessionForTask(ev.TaskID)
	if ref.session == nil {
		return nil // progress noise, not a deliverable: nothing to account for
	}

	var msg string
	switch ev.EventType {
	case "EventHeartbeatSleep":
		secs, _ := ev.Payload["sleep_seconds"].(float64)
		msg = fmt.Sprintf("💤 Task sleeping for %ds (self-paced heartbeat)", int(secs))
	case "EventHeartbeatComplete":
		msg = "✅ Task completed via heartbeat"
	case "EventHeartbeatFail":
		msg = "❌ Task failed via heartbeat"
	default:
		return nil
	}

	ref.session.AddEvent(chat.ChatEvent{
		Type: "heartbeat",
		Data: msg,
		Meta: map[string]any{
			"task_id": ev.TaskID,
			"step_id": ev.StepID,
			"event":   ev.EventType,
			"turn":    ev.Payload["turn"],
		},
	})
	// Persist, like every other session-mutating callback here. Without this a
	// heartbeat only exists in the in-memory ring, so a client that reconnects
	// after a restart (or a second process serving the same session) never
	// sees it.
	h.persistSession(ref.session)
	return nil
}

// notifyDecisionRequired tells the user a task is blocked waiting for them.
//
// This one is pushed even when no session can be resolved: a blocked task
// cannot resolve itself, so it needs a human regardless of where it came from,
// and the queue's admin fallback (chat_telegram_outbox.go) is what reaches one.
func (h *ChatHandler) notifyDecisionRequired(ev obs.AgentEvent) error {
	taskID := ev.TaskID
	ref := h.resolveSessionForTask(taskID)

	decisionID, _ := ev.Payload["decision_id"].(string)
	dtype, _ := ev.Payload["type"].(string)
	options := stringSlice(ev.Payload["options"])
	text := fmt.Sprintf("Decision required for task %s (step %s)", taskID, ev.StepID)
	meta := map[string]any{
		"task_id":       taskID,
		"decision_id":   decisionID,
		"step_id":       ev.StepID,
		"options":       options,
		"decision_type": dtype,
	}

	if ref.session == nil {
		h.logNotificationDrop(taskID, kindDecisionNeeded, ref.dropReason, ref.sessionID)
	} else {
		ref.session.AddEvent(chat.ChatEvent{Type: kindDecisionNeeded, Data: text, Meta: meta})
		h.persistSession(ref.session)

		// Waking needs a session: AddEvent above feeds the SSE queue, but the
		// agent never sees it. A task blocked on a decision cannot resolve
		// itself, so wake the model -- it can ask the user, which is the only
		// path forward (see chat_wake.go).
		//
		// The dedupe key covers both payload shapes: the gate sends a
		// decision_id, the max-turns path sends reason only.
		key := firstNonEmpty(decisionID, str(ev.Payload["reason"]), "step_"+ev.StepID)
		h.wakeMain(ref.sessionID, taskID, "decision", decisionWakeText(taskID, ev.StepID, ev.Payload), key)
	}
	h.queueNotification(taskID, ref.sessionID, kindDecisionNeeded, decisionDedupeKey(taskID, decisionID, ev), text, meta)
	return nil
}

// decisionDedupeKey keeps one blocked decision to one notification per task
// even when several hubs see the same event. The max-turns path sends no
// decision_id, so it falls back to the step, which is what identifies it.
func decisionDedupeKey(taskID, decisionID string, ev obs.AgentEvent) string {
	return "decision_needed|" + taskID + "|" + firstNonEmpty(decisionID, "step_"+ev.StepID)
}

// isTelegramSession reports whether a session id names a Telegram chat, i.e.
// whether some hub can push to the user behind it without being asked.
func isTelegramSession(sessionID string) bool {
	return strings.HasPrefix(sessionID, "telegram_")
}

// queueNotification puts one outbound notification on the durable queue for a
// transport to deliver. It is additive: the live SSE event and the history
// message above still happen, so a client that is watching now notices no
// difference. What the queue adds is the case where nobody is watching from
// this process, and the case where the notification was produced by a hub that
// cannot speak to the user at all.
func (h *ChatHandler) queueNotification(taskID, sessionID, kind, dedupeKey, text string, meta map[string]any) {
	if h.Outbox == nil {
		return // file-backed sessions have no DB to queue on
	}

	var metaJSON string
	if len(meta) > 0 {
		b, err := json.Marshal(meta)
		if err != nil {
			// The text alone still reaches the user, so this is a downgrade,
			// not a loss: an inline keyboard degrades to a plain notice.
			log.Printf("WARN: chat: notification %s meta would not encode: %v", dedupeKey, err)
		} else {
			metaJSON = string(b)
		}
	}

	added, err := h.Outbox.Enqueue(context.Background(), store.OutboxItem{
		SessionID: sessionID,
		Kind:      kind,
		DedupeKey: dedupeKey,
		Text:      text,
		MetaJSON:  metaJSON,
	})
	switch {
	case err != nil:
		h.logOutbox(eventNotificationDropped, map[string]any{
			"task_id": taskID, "kind": kind, "reason": "enqueue_failed",
			"session_id": sessionID, "error": err.Error(),
		})
		log.Printf("[chat] could not queue %s notification for task %s: %v", kind, taskID, err)
	case added:
		h.logOutbox(eventNotificationQueued, map[string]any{
			"kind": kind, "dedupe_key": dedupeKey, "session_id": sessionID,
		})
	default:
		// Already queued, so a producer retried after losing its own write.
		h.logOutbox(eventNotificationDuplicate, map[string]any{
			"kind": kind, "dedupe_key": dedupeKey, "session_id": sessionID,
		})
	}
}

// logNotificationDrop states in the log what used to be an unlogged return.
// Measured 2026-11-06: 9 of the 12 most recent delegated tasks carried no
// session id and one pointed at a session that was no longer stored, so their
// completion notifications disappeared with no trace, and an operator reading
// the log could not tell "nothing happened" from "it happened in another hub".
func (h *ChatHandler) logNotificationDrop(taskID, kind, reason, sessionID string) {
	h.logOutbox(eventNotificationDropped, map[string]any{
		"task_id": taskID, "kind": kind, "reason": reason, "session_id": sessionID,
	})
	log.Printf("[chat] queued no %s notification for task %s: %s", kind, taskID, reason)
}

// logOutbox writes one line to the outbox sink, tolerating the nil App of a
// handler built without a running application (tests, harness-bench).
func (h *ChatHandler) logOutbox(event string, detail map[string]any) {
	if h.App == nil || h.App.Logger == nil {
		return
	}
	h.App.Logger.Log(obs.EventType(event), chatOutboxSink, "", detail)
}
