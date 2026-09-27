# Changelog

All notable changes to this project are documented here. The format is based on
Keep a Changelog, and this project adheres to Semantic Versioning.

## [0.1.0] - unreleased

Initial open-source release of Vortex, a headless MCP orchestration engine.

### Added
- MCP server (stdio + SSE transport)
- Multi-model routing (Anthropic, OpenAI, Gemini, Ollama, DeepSeek)
- DAG task graph execution with parallel branches
- Experience store + failure-mode correction
- Code intelligence (cross-package symbol/impact analysis)
- JIT code execution sandbox (Python/Node/Bun/Lua)
- CLI execution mode
- `ListSessions` tool for querying active JIT sessions
- `orchestrator_default` role with automatic fallback when configured role is missing
- `root_cause` classification on step failure events for structured failure analysis
- Content-addressed cookbook cache with deduplication
- `GlobalEventLogger` and `ReplayScheduler` enabled by default for trajectory audit
- `manifest.json` always persisted for task history visibility
- Tool argument JSON Schema pre-flight validation (`core/tool_validation.go`)
- Task ownership guard (`DenyIfNotOwner`) with composite caller identity
- Batch query interface (`GetBatch`) and SQLite `LoadBatch` for store performance
- UserProfile injection and reflection-based extraction from memory bank
- `SuspendTask` / `ResumeSuspendedTask` control plane operations
- `SetMemoryBankStore` wiring for automatic profile extraction
- ScriptProvider with environment variable allowlist and `fs.read` bridge
- `CancelTask` cascade test coverage

### Fixed
- Data races in scheduler, event bus, JIT session, and MCP handshake (mutex protection, goroutine leak cleanup)
- `executeStep` panic recovery to prevent scheduler crashes
- `persistGraph` re-enabled with locked/unlocked split to avoid recursive deadlock
- `handleOutput` extracted into phase functions for testability
- Response payload trimming to prevent oversized MCP responses
- `GrayscaleController` respects `trialRate=0.0` (changed guard from `<= 0` to `< 0`)
- HTTP response body closing on all paths in `RemoteTaskHub`
- Path traversal rejection in admin deploy (audit S-C2)
- Constant-time comparison for auth token validation
- Environment variable prefix consistency (`VORTEX_` not `ORCHESTRATOR_`)

### Changed
- Scheduler decision logic refactored into `scheduler_decision_*` files
- `tryDefaultRoleFallback` for graceful role resolution
