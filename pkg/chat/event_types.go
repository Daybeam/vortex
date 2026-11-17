package chat

// Event type constants for ChatEvent.Type.
//
// Two families:
//   - Chat family: existing event types emitted by ChatHarness.Run and
//     consumed by forwardEvents (TG) and StreamHandler (web SSE).
//   - Modality family: new event types for non-text output modalities
//     (speech, action, artifact, decision, progress). These have no
//     consumers yet — they are defined so future sinks can subscribe
//     without modifying the event structure. See
//     docs/plan/CHAT_MODALITY_AND_ACTOR_DESIGN.md §2.1 (Patch A).
//
// The comment at chat_harness.go:51 ("delta | tool_call | tool_result |
// done | error") was already stale — forwardEvents handles 20+ types.
// These constants replace string literals to prevent typos and make the
// full set discoverable at compile time.

// Chat family — existing event types.
const (
	EvDelta             = "delta"
	EvThinking          = "thinking"
	EvToolCall          = "tool_call"
	EvToolResult        = "tool_result"
	EvToolError         = "tool_error"
	EvDone              = "done"
	EvError             = "error"
	EvSlashCommand      = "slash_command"
	EvSteer             = "steer"
	EvLoopGuard         = "loop_guard"
	EvPlanDrift         = "plan_drift"
	EvTruncationViolation = "truncation_violation"
	EvResultRepetition  = "result_repetition"
	EvRepeatedWrite     = "repeated_write"
	EvFollowupTurn      = "followup_turn"
	EvHookWarning       = "hook_warning"
	EvHookAbort         = "hook_abort"
	EvHeartbeat         = "heartbeat"
	EvDecisionNeeded    = "decision_needed"
	EvTaskCallback      = "task_callback"
	// Contract family — the chat path's only quality signals. They were emitted
	// as string literals from chat_harness.go and chat_completion_check.go and
	// were missing from this registry, which is the file that claims to make
	// "the full set discoverable at compile time". The values are the wire
	// format: `_check_events.py` and the SSE frontends match on them, so a
	// rename here is a protocol change, not a refactor.
	EvArtifactsComplete   = "artifacts_complete"
	EvArtifactsIncomplete = "artifacts_incomplete"
	EvCompletionCheck     = "completion_check"
)

// Modality family — new event types for non-text output (Patch A).
// Structured payloads go in ChatEvent.Meta; Data holds a human-readable
// summary only. Consumers that don't recognise these types silently
// skip them (forwardEvents has no default branch; SSE frontends ignore
// unknown event: names).
const (
	EvSpeech   = "speech"   // Data=text; Meta={text,emotion,voice,interruptible}
	EvAction   = "action"   // Data=summary; Meta={kind,x,y,key,window,action_id}
	EvArtifact = "artifact" // Data=description; Meta={path,mime,size,kind}
	EvDecision = "decision" // Data=prompt; Meta={decision_id,options,default}
	EvProgress = "progress" // Data=label; Meta={current,total,phase}
)

// IsTerminalEvent reports whether an event type indicates the
// conversation turn is complete and the event loop should exit.
// waitForTask modifies the semantics of EvDone: when true, EvDone
// fires at task submission time (not completion), so the loop must
// keep polling for EvTaskCallback.
func IsTerminalEvent(evType string, waitForTask bool) bool {
	switch evType {
	case EvSlashCommand, EvError, EvTaskCallback:
		return true
	case EvDone:
		return !waitForTask
	}
	return false
}
