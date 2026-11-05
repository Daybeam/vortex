package chat

// DefaultMaxTurns is the chat agent-loop budget when nothing configures it.
//
// 50 as of 2026-11-04. The previous 6 ended turns in the middle of multi-step
// work, and unlike the DAG path the chat loop has no way to pick a run back up:
// running out returns "max turns (N) exceeded" (chat_harness.go:1132-1133) and
// the task is over. A larger ceiling is the only mitigation until resume exists.
//
// Both entry points use it — the loop's own zero-value fallback
// (chat_harness.go:491-493) and pkg/api/chat_handlers.go, where config
// chat_max_turns / max_tool_turns can override it.
const DefaultMaxTurns = 50
