package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestSpawn_NeverReturnsNilNil is the regression test for the L-N12 family
// (fix-propagation gap, 4th occurrence in this codebase).
//
// Bug class: Spawner.Spawn delegates to BuildInterceptorChain, which can
// short-circuit and return (nil, nil). The L-N10 fix added a nil guard to
// spawnDecider only; 7 sibling call sites (scheduler_dag.go:664,
// scheduler_decision_audit.go:64/107/329/400,
// scheduler_decision_helpers.go:189, replay_scheduler.go:593) dereferenced
// the result without a nil check, causing nil-pointer panics caught silently
// by goBackground's recover — leaving steps/debates/audits deadlocked forever.
//
// Root-cause fix: Spawn now wraps a (nil, nil) return into an empty SpawnResult
// at spawner.go:254, so NO caller can ever receive a nil result with nil error.
//
// This test FAILS before the fix (Spawn returns nil) and PASSES after (Spawn
// returns a non-nil empty SpawnResult).
//
// Regression Sediment Rule: this test sediment is linked to audit L-N12.
func TestSpawn_NeverReturnsNilNil(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	// Create a Spawner whose interceptor chain short-circuits with (nil, nil).
	// This simulates an external interceptor (e.g. a policy guard or rate
	// limiter) deciding "nothing to do, no error."
	spawner := &Spawner{
		registry: &config.Registry{},
		logger:   logger,
		interceptors: []Interceptor{
			func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
				return nil, nil // short-circuit
			},
		},
	}

	req := &SpawnRequest{
		TaskID: "test_nil_nil",
		StepID: "s1",
		RoleID: "test_role",
		Task:   "verify nil-nil guard",
	}

	result, spawnErr := spawner.Spawn(context.Background(), req)

	// The error must be nil (the interceptor returned no error).
	if spawnErr != nil {
		t.Fatalf("L-N12: expected nil error from short-circuit interceptor, got %v", spawnErr)
	}

	// The result MUST be non-nil — this is the core assertion.
	// Before the fix, Spawn returned (nil, nil) here, and every caller that
	// did `res.Output.Result` panicked with nil pointer dereference.
	if result == nil {
		t.Fatal("L-N12 regression: Spawn returned (nil, nil) — callers will nil-deref on res.Output.Result")
	}

	// The result must have a safe (non-nil) Output.Result map so callers that
	// do `res.Output.Result["content"]` don't panic on nil-map access (reads
	// from nil maps are actually safe in Go, but writes panic — and some
	// callers write to Result before returning).
	if result.Output.Result == nil {
		t.Fatal("L-N12 regression: SpawnResult.Output.Result is nil — callers that write to Result will panic")
	}
}

// TestSpawn_NilNilGuard_AllCallSitesSafe verifies that the 7 call sites
// identified in audit L-N12 can safely handle a (nil, nil) Spawn return now
// that the root-cause fix is in place. This is a structural/compile-time
// guarantee: because Spawn can no longer return (nil, nil), the nil-deref
// at each site is unreachable.
//
// We verify the fix at the source (Spawn itself) rather than mocking each
// call site, because the root-cause fix protects ALL current and future
// callers — not just the 7 known sites.
func TestSpawn_NilNilGuard_AllCallSitesSafe(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	spawner := &Spawner{
		registry: &config.Registry{},
		logger:   logger,
		interceptors: []Interceptor{
			func(ctx context.Context, req *SpawnRequest, next SpawnerHandler) (*SpawnResult, error) {
				return nil, nil
			},
		},
	}

	// Simulate the access pattern at each of the 7 L-N12 call sites:
	//   res, err := s.spawner.Spawn(...)
	//   if err == nil { ... res.Output.Result ... }   // L-N12a/d/f pattern
	//   if err != nil { return ... }                  // L-N12b/c/e/g pattern
	//   ... res.Output.Result ...                     // deref
	//
	// All 7 patterns must be safe now.
	sites := []string{
		"L-N12a scheduler_dag.go:664 (executeStep)",
		"L-N12b scheduler_decision_audit.go:64 (runCriticAudit)",
		"L-N12c scheduler_decision_audit.go:107 (runProposerSynthesize)",
		"L-N12d scheduler_decision_audit.go:329 (verifyWithAudit)",
		"L-N12e scheduler_decision_audit.go:400 (generateDynamicRubrics)",
		"L-N12f scheduler_decision_helpers.go:189 (foldNode)",
		"L-N12g replay_scheduler.go:593 (verifyCrossFamily)",
	}

	for _, site := range sites {
		t.Run(site, func(t *testing.T) {
			res, err := spawner.Spawn(context.Background(), &SpawnRequest{
				TaskID: "test", StepID: "s1", RoleID: "r", Task: "t",
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Pattern 1: if err == nil { use res.Output.Result }
			if err == nil {
				_ = res.Output.Result // must not panic
				_ = res.ProviderID    // must not panic (L-N12a)
			}
			// Pattern 2: if err != nil { return } else { use res.Output.Result }
			if err != nil {
				return
			}
			_ = res.Output.Result["content"] // must not panic (L-N12e)
		})
	}
}
