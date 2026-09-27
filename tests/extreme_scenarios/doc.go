// Package extreme_tests contains simulation tests for extreme user scenarios.
// These tests run as part of the regular test suite (go test ./...).
//
// Scenarios covered:
//   - Super-large document input rejection (InputGuard)
//   - Multi-document asset handling without OOM (AssetManager)
//   - Super-large context triggering Fork (ContextManager)
//   - 100+ concurrent task stress (Scheduler)
//   - Goroutine leak detection
//   - Disk full / I/O error simulation
//   - Corrupted experience store graceful handling
//   - SQLite write contention (SQLITE_BUSY)
//   - SSE mid-stream disconnect + reconnect
//   - Rapid config changes mid-task
package extreme_tests
