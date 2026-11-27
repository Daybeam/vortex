package core

import (
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// spawnLoopState is the working memory for one doSpawn invocation. Keeping it
// together makes the turn-loop ownership explicit: none of these counters or
// slices survive the step, and every field retains the original zero value.
type spawnLoopState struct {
	trace         []store.ToolInteraction
	decisionIDs   []string
	statesVisited []string
	previousState string

	toolFailCount     map[string]int
	toolCallCount     map[string]int
	resultStreak      map[string]int
	resultLastHash    map[string]uint64
	sieveGuardianHits int
	zeroEffectNudged  bool
	midAbandonNudged  bool

	lastResultHash           uint64
	consecutiveRepeatCount   int
	outputStagnationHinted   bool
	stagnationGraceTurns     int
	stagnationHintAt         int
	echoGuard                *EchoGuard
	consecutiveParseFailures int
	fsOnlyTurnStreak         int
	fsOnlyStreakLimit        int

	effectiveProviderID string
	effectiveModelID    string
	emptyRespRetries    int
	lastMCPServers      []providers.MCPServerDef
	stickyContext       string
	calledTools         map[string]bool
	userBlocks          []schemas.ContentBlock
	bindings            []config.MCPBinding
	vdaAttachments      []schemas.Attachment
}

func newSpawnLoopState(s *Spawner, userBlocks []schemas.ContentBlock) *spawnLoopState {
	return &spawnLoopState{
		toolFailCount: make(map[string]int),
		// #11 (eval §8.41): per-step identical-call counter. Two identical
		// calls are allowed; the third requires a _reason argument.
		toolCallCount: make(map[string]int),
		// #11b: consecutive byte-identical answers, not cumulative repeats.
		resultStreak:         make(map[string]int),
		resultLastHash:       make(map[string]uint64),
		stagnationGraceTurns: s.registry.System.FeatureFlags.StagnationGraceTurns,
		echoGuard:            NewEchoGuard(s.registry.System.FeatureFlags.StagnationGraceTurns),
		fsOnlyStreakLimit:    s.registry.System.FeatureFlags.FSOnlyStreakLimit,
		calledTools:          make(map[string]bool),
		userBlocks:           userBlocks,
	}
}
