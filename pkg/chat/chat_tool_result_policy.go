package chat

import "github.com/daybeam/vortex/core/sieve"

// toolResultPolicy decides what the chat harness does with a successful tool
// result. It exists as a pure function because the three branches were an
// if/else-if/else chain inside the tool loop, and the only way to test them was
// to drive a whole turn through a fake provider.
//
// The classification is the thing that was wrong: `orchestrator_task_status`
// carries none of the poller name signals (poll/progress/health/wait) and does
// not start with a read verb, so it fell through to actionResult — the engine
// announced it as "[ACTION COMPLETED]", invalidated the read cache on every
// poll, and let the loop guard count consecutive polls as an infinite loop.
type toolResultPolicy int

const (
	// cacheReadResult: the result is a read, cache it for replay and watch for
	// identical payloads from different queries.
	cacheReadResult toolResultPolicy = iota
	// alwaysFreshResult: the answer changes on its own, so never cache it and
	// never invalidate the cache because of it.
	alwaysFreshResult
	// actionResult: the call mutated state, so every cached read is stale.
	actionResult
)

func toolResultPolicyFor(name string, args map[string]any) toolResultPolicy {
	if sieve.IsPollingCall(name, args) {
		return alwaysFreshResult
	}
	if !sieve.IsReadTool(name) {
		return actionResult
	}
	return cacheReadResult
}
