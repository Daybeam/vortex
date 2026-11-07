package api

import (
	"fmt"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
)

// ── Background-task progress forwarding (ADDED 2026-10-30) ───────────
//
// WHY THIS EXISTS
//
// `delegate_to_orchestrator` is deliberately asynchronous: it returns a task
// id immediately and the work continues in the scheduler. Measured on the
// harness-bench entrypoint (T2, 2026-10-30), the chat stream therefore looks
// like this for a delegated task:
//
//	seq 1 tool_call        delegate_to_orchestrator
//	seq 2 tool_result      "Delegated to orchestrator task task_xxx"
//	seq 3-4 delta          main agent signs off
//	seq 5 done             ← chat turn over, SSE returns, connection closes
//	   …  nothing at all while the task runs  …
//	seq 8 task_callback    ← injected by WireTaskCompletionCallbacks
//
// The gap is invisible. The VCP self-driven heartbeat (core/heartbeat.go) was
// built exactly to fill it, but it is model-driven: it only fires when the
// model itself writes `[[Heartbeat::Sleep::N]]` into its response text, and
// that protocol is documented nowhere the model can see it — no system prompt,
// no tool description, no injected instruction. Grep across the whole tree
// returns only heartbeat.go's own comments and docs/plan/VCP_HEARTBEAT_AND_
// CONTEXT_TIERING_DESIGN.md. A protocol the model was never told about is
// emitted zero times, so in practice the heartbeat has never fired.
//
// This file adds the half that does not depend on model behaviour: the engine's
// own per-task events (every step transition, every provider round) are already
// published to obs.DefaultBus by Logger.Log, so they can be projected into the
// chat session as "progress" ChatEvents. That guarantees the user sees *some*
// status within one round of delegating, whatever the model does.
//
// BUDGET: ChatSession keeps a 256-event ring (core/chat_session.go). Step
// transitions are bounded by the graph size; round ticks are throttled to one
// per progressTickInterval per task. A 30-minute task therefore adds ~60
// events, not one per round.

// progressTickInterval bounds round-level ("still alive") progress events.
// Step transitions are not throttled — they are bounded by the number of steps.
const progressTickInterval = 30 * time.Second

// wireTaskProgress subscribes the handler to the task lifecycle events that
// carry useful state for a waiting user. Called once from
// WireTaskCompletionCallbacks; nil-safe when the scheduler is absent.
func (h *ChatHandler) wireTaskProgress() {
	forward := func(ev obs.AgentEvent) error {
		h.forwardTaskProgress(ev)
		return nil
	}
	for _, t := range []string{
		string(obs.EventTaskSubmitted),
		string(obs.EventStepCompleted),
		string(obs.EventStepFailed),
		string(obs.EventProviderCallStarted),
	} {
		obs.DefaultBus.Subscribe(t, forward)
	}
}

// forwardTaskProgress projects one engine event into the owning chat session.
// Best-effort: any missing link (no task, no session, no status) is a silent
// no-op, because this is an observability nicety and must never fail a task.
func (h *ChatHandler) forwardTaskProgress(ev obs.AgentEvent) {
	taskID := ev.TaskID
	if taskID == "" {
		return
	}
	if h.App == nil || h.App.Scheduler == nil {
		return
	}

	var msg string
	switch ev.EventType {
	case string(obs.EventTaskSubmitted):
		msg = fmt.Sprintf("🆔 Background task %s accepted — query it later by this id.", taskID)

	case string(obs.EventStepCompleted):
		msg = fmt.Sprintf("✅ Task %s · step %s completed.", taskID, ev.StepID)

	case string(obs.EventStepFailed):
		msg = fmt.Sprintf("❌ Task %s · step %s failed", taskID, ev.StepID)
		if e, _ := ev.Payload["error"].(string); e != "" {
			if len(e) > 200 {
				e = e[:200] + "…"
			}
			msg += ": " + e
		} else {
			msg += "."
		}

	case string(obs.EventProviderCallStarted):
		// Round-level liveness. Throttled: the first round of a task always
		// passes (no prior timestamp), later rounds at most once per interval.
		now := time.Now()
		if v, ok := h.progressLast.Load(taskID); ok {
			if last, ok := v.(time.Time); ok && now.Sub(last) < progressTickInterval {
				return
			}
		}
		h.progressLast.Store(taskID, now)
		turn, _ := ev.Payload["turn"].(float64)
		msg = fmt.Sprintf("⏳ Task %s · step %s · round %d", taskID, ev.StepID, int(turn))

	default:
		return
	}

	// Resolve the owning chat session. Same lookup as the completion callback:
	// the task graph carries SessionID, and only chat-submitted tasks have one
	// (plain MCP callers get no progress events, which is correct — there is no
	// session to inject into).
	statusData, ok := h.App.Scheduler.GetStatus(taskID, "full")
	if !ok || statusData == nil {
		return
	}
	sessionID, _ := statusData["session_id"].(string)
	if sessionID == "" {
		return
	}
	session := h.Store.Get(sessionID)
	if session == nil {
		return
	}

	session.AddEvent(chat.ChatEvent{
		Type: "progress",
		Data: msg,
		Meta: map[string]any{
			"task_id":     taskID,
			"step_id":     ev.StepID,
			"event":       ev.EventType,
			"round":       ev.Payload["turn"],
			"task_status": string(schemas.GraphRunning),
		},
	})
	h.persistSession(session)
}
