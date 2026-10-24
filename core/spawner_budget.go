package core

// defaultSpawnOutputTokens is the response cap for a spawn that does not ask
// for its own. It covers everything the model emits, and on a thinking model
// that includes reasoning tokens, which never reach response text.
//
// 8192, not the original 4096. Measured 2026-10-20 (gemini-3-flash-preview via
// an OpenAI-compatible gateway): three responses hit the cap at 4068/4069/3932
// completion tokens where the last emitted zero characters and zero tool calls
// — the whole budget was reasoning. The old value was sized for prose answers,
// which is the wrong unit once reasoning shares the same budget.
//
// Also used by the single-shot generation calls in this package that emit a
// structured document (role generation, reflection's Lua script), because they
// are subject to exactly the same failure: reasoning consumes the budget before
// the document starts. Keep one constant rather than three literals that drift.
const defaultSpawnOutputTokens = 8192

// defaultDedupHardCap is the reason-blind ceiling for identical calls inside
// one step, used when system.dedup_hard_cap is unset.
//
// Chosen at 5 because #11 already blocks the 3rd repeat for a model that does
// not explain itself; the cap only has to catch the model that *does*. Two
// extra calls is enough slack for a genuine retry (data refresh, transient
// error) without giving a looping model a budget worth burning.
const defaultDedupHardCap = 5

// dedupHardCap resolves the ceiling for one spawn, mirroring spawnOutputBudget:
// deployment setting first, built-in default second.
func (s *Spawner) dedupHardCap() int {
	if s != nil && s.registry != nil {
		s.registry.Mu.RLock()
		configured := s.registry.System.DedupHardCap
		s.registry.Mu.RUnlock()
		if configured > 0 {
			return configured
		}
	}
	return defaultDedupHardCap
}

// spawnOutputBudget resolves the response token cap for one spawn.
//
// Precedence: an explicit per-spawn request, then the deployment setting, then
// the built-in default. The middle step exists because the right cap depends on
// the model, not on this code: a thinking model needs room for reasoning that a
// prose-tuned constant cannot anticipate.
func (s *Spawner) spawnOutputBudget(req *SpawnRequest) int {
	if req != nil && req.MaxOutputTokens > 0 {
		return req.MaxOutputTokens
	}
	if s != nil && s.registry != nil {
		s.registry.Mu.RLock()
		configured := s.registry.System.DefaultSpawnOutputTokens
		s.registry.Mu.RUnlock()
		if configured > 0 {
			return configured
		}
	}
	return defaultSpawnOutputTokens
}
