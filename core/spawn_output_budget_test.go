package core

import "testing"

// TestSpawnOutputBudget_DefaultsToThinkingSafeCap pins the raised default.
//
// 4096 was sized for prose. On a thinking model the reasoning tokens share that
// same budget while producing no visible output, so 4096 left the answer with
// nothing: measured 2026-10-20, a response hit the old cap at 3932 completion
// tokens with zero text characters and zero tool calls, and the step then spun
// on a repeated call until the loop guard aborted it.
func TestSpawnOutputBudget_DefaultsToThinkingSafeCap(t *testing.T) {
	if defaultSpawnOutputTokens <= 4096 {
		t.Fatalf("default cap %d leaves no room for reasoning on top of a prose answer", defaultSpawnOutputTokens)
	}
	var s *Spawner
	if got := s.spawnOutputBudget(&SpawnRequest{}); got != defaultSpawnOutputTokens {
		t.Errorf("nil-registry spawner: got %d, want %d", got, defaultSpawnOutputTokens)
	}
	if got := (*Spawner)(nil).spawnOutputBudget(nil); got != defaultSpawnOutputTokens {
		t.Errorf("nil request and nil spawner: got %d, want %d", got, defaultSpawnOutputTokens)
	}
}

// TestSpawnOutputBudget_PerRequestWins keeps the existing precedence: a caller
// that knows its answer is long (the planner) must not be capped by the
// deployment default.
func TestSpawnOutputBudget_PerRequestWins(t *testing.T) {
	s := &Spawner{}
	req := &SpawnRequest{MaxOutputTokens: 24000}
	if got := s.spawnOutputBudget(req); got != 24000 {
		t.Errorf("got %d, want the request's 24000", got)
	}
}

// TestSpawnOutputBudget_ZeroRequestIsNotAZeroCap guards against a request that
// leaves MaxOutputTokens unset silently requesting a zero-token response.
func TestSpawnOutputBudget_ZeroRequestIsNotAZeroCap(t *testing.T) {
	s := &Spawner{}
	if got := s.spawnOutputBudget(&SpawnRequest{MaxOutputTokens: 0}); got <= 0 {
		t.Errorf("an unset per-request cap produced %d", got)
	}
}
