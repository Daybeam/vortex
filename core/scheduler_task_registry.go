package core

import "github.com/daybeam/vortex/store"

// SetTaskRegistry installs the task-level ledger after construction.
//
// Why a setter instead of a plain field write: the engine is built before the
// store is wired (`scheduler := NewDirectedEngine(...)` then
// `scheduler.TaskRegistry = ...` in main*.go), but the Asset Sentry goroutine
// starts *inside* the constructor. That goroutine read the field while main
// assigned it — a data race, measured 2026-11-05 under `go test -race`:
//
//	Write at 0x... by goroutine 13:
//	  TestMarkTaskAborted_FailsLoudlyWithoutRegistry()
//	      /src/core/scheduler_taskctl_aborted_test.go:82
//	Previous read at 0x... by goroutine 15:
//	  NewDirectedEngine.func1()
//	      /src/core/scheduler.go:311
//
// The read appeared on 2026-11-02 (e28fa5a7c, D-GC-1) when Sentry's argument
// changed from a constructor-captured local (`ts`) to this field. Capturing the
// field at construction would be wrong for a different reason: it is nil then,
// and the ledger is wired immediately afterwards, so the sweep would run with a
// nil registry forever (one tick per hour — invisible).
//
// All access goes through SetTaskRegistry / taskRegistry. Direct field writes
// are still possible for compatibility but must not happen while the engine is
// running.
func (s *DirectedEngine) SetTaskRegistry(tr store.TaskRegistry) {
	s.taskRegistryMu.Lock()
	s.TaskRegistry = tr
	s.taskRegistryMu.Unlock()
}

// taskRegistry returns the ledger currently wired, or nil before wiring.
func (s *DirectedEngine) taskRegistry() store.TaskRegistry {
	s.taskRegistryMu.RLock()
	defer s.taskRegistryMu.RUnlock()
	return s.TaskRegistry
}
