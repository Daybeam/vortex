package api

import "sort"

// The per-session ledger of tasks a chat turn handed off.
//
// Why it is not on the harness: the poll lever (VORTEX_POST_DELEG_POLL=wake) has
// to answer the first status check and lock the rest, and the checks that
// matter happen in the FOLLOW-UP turn. That turn is a fresh ChatHarness cloned
// from the handler's template, so the instance that delegated no longer exists
// and its `pd` state went with it. Measured 2026-11-11 on the wake arm: the
// single orchestrator_task_status call of the run landed in `_fu1`, where
// `post_deleg_polls` read 0 — the lever was blind exactly where the polling was.
//
// Why a set and not "the last id": one turn can delegate more than once. The
// same measurement showed `delegated=2`, two status calls in the trajectory, and
// `post_deleg_polls=1` — the check for the first task matched nothing, so it was
// neither counted nor blocked.
//
// Keyed the same way as consumedTasks ("sessionID|taskID") so both ledgers are
// drained with the same reasoning, and in memory only: a wake turn is spawned by
// this process, so a restart has no turn left to seed.

// noteDelegatedTask records that `sessionID` handed `taskID` to the
// orchestrator. Called from the per-request Delegate wrapper, which is the one
// place that sees the id.
func (h *ChatHandler) noteDelegatedTask(sessionID, taskID string) {
	if h == nil || sessionID == "" || taskID == "" {
		return
	}
	h.delegatedTasks.Store(sessionID+"|"+taskID, struct{}{})
}

// delegatedTasksFor returns the task ids `sessionID` has handed off, sorted so
// logs and tests are stable. Empty when nothing was delegated, which keeps the
// wake turn's behaviour identical to before the ledger existed.
func (h *ChatHandler) delegatedTasksFor(sessionID string) []string {
	if h == nil || sessionID == "" {
		return nil
	}
	prefix := sessionID + "|"
	var ids []string
	h.delegatedTasks.Range(func(k, _ any) bool {
		key, ok := k.(string)
		if !ok || len(key) <= len(prefix) || key[:len(prefix)] != prefix {
			return true
		}
		ids = append(ids, key[len(prefix):])
		return true
	})
	sort.Strings(ids)
	return ids
}

// forgetDelegatedTasks drops the session's ledger. Called when a session is
// deleted so a recycled id cannot inherit another conversation's hand-offs.
func (h *ChatHandler) forgetDelegatedTasks(sessionID string) {
	if h == nil || sessionID == "" {
		return
	}
	prefix := sessionID + "|"
	h.delegatedTasks.Range(func(k, _ any) bool {
		if key, ok := k.(string); ok && len(key) > len(prefix) && key[:len(prefix)] == prefix {
			h.delegatedTasks.Delete(k)
		}
		return true
	})
}
