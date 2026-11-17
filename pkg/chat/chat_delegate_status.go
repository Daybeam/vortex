package chat

// isDelegatedTaskTerminal reports whether the delegated task has reached a
// terminal status (completed, failed, cancelled). It reads the current state
// without blocking. When DelegateStatus is nil or the status is unavailable,
// it returns false — conservative so that artifacts_complete does not fire on
// stale or missing data.
//
// D5 (2026-11-08): the artifacts_complete predicate used to fire as soon as
// all declared artifacts existed, regardless of whether the delegated task was
// still running. That meant artifacts written early could trigger a premature
// completion signal. The caller guards with `h.DelegateStatus == nil || …`
// so non-delegated chat sessions keep the old behavior.
func (h *ChatHarness) isDelegatedTaskTerminal(taskID string) bool {
	if h.DelegateStatus == nil {
		return false
	}
	st, ok := h.DelegateStatus(taskID, 0)
	if !ok || st == nil {
		return false
	}
	status, _ := st["status"].(string)
	return isTerminalChatStatus(status)
}
