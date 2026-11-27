package core

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

const maxConsecutiveDuplicates = 3

// executeSpawnToolTurn owns the post-response tool phase, including guardian,
// confirmation, stagnation, VDA, progressive rerouting and history append.
// A nil result/error means the outer turn loop should continue.
func (s *Spawner) executeSpawnToolTurn(ctx context.Context, req *SpawnRequest, role *config.Role, prep *spawnPreparation, st *spawnLoopState, resp *providers.ProviderResponse, pCfg *config.ProviderConfig, turn, maxTurns int, turnCtx *TurnIdempotencyContext, turnDecisionID string) (*SpawnResult, error) {
	var toolResults []string
	var turnAttachments []schemas.Attachment
	needsVerification := false

	sieveSt := &sieveTurnState{sieveGuardianHits: st.sieveGuardianHits, userBlocks: st.userBlocks}
	sieveOut := s.sieveGuardianTurn(ctx, req, role, resp, st.trace, turn, pCfg,
		st.effectiveProviderID, st.effectiveModelID, st.statesVisited, sieveSt)
	st.sieveGuardianHits, st.userBlocks = sieveSt.sieveGuardianHits, sieveSt.userBlocks
	if sieveOut.done {
		return sieveOut.result, nil
	}
	if sieveOut.skip {
		return nil, nil
	}

	tcState := &toolExecState{
		toolResults: toolResults, trace: st.trace, toolCallCount: st.toolCallCount,
		toolFailCount: st.toolFailCount, resultStreak: st.resultStreak, resultLastHash: st.resultLastHash,
		needsVerification: needsVerification, turnAttachments: turnAttachments,
		statesVisited: st.statesVisited, previousState: st.previousState,
	}
	s.processToolCalls(ctx, req, resp, st.bindings, turnDecisionID, turnCtx, tcState)
	st.trace = tcState.trace
	st.toolCallCount = tcState.toolCallCount
	st.toolFailCount = tcState.toolFailCount
	st.resultStreak = tcState.resultStreak
	st.resultLastHash = tcState.resultLastHash
	st.statesVisited = tcState.statesVisited
	st.previousState = tcState.previousState
	toolResults = tcState.toolResults
	turnAttachments = tcState.turnAttachments
	needsVerification = tcState.needsVerification

	for _, tc := range resp.ToolCalls {
		st.calledTools[tc.Name] = true
	}

	if len(toolResults) > 0 {
		turnHash, shouldTrack := computeTurnHash(resp.ToolCalls, toolResults)
		if shouldTrack {
			if turnHash == st.lastResultHash {
				st.consecutiveRepeatCount++
			} else {
				st.lastResultHash = turnHash
				st.consecutiveRepeatCount = 1
			}
			hintText, kill := s.handleStagnationTurn(req, turn, st.consecutiveRepeatCount,
				st.stagnationGraceTurns, st.stagnationHintAt, maxConsecutiveDuplicates,
				st.outputStagnationHinted, st.trace, role.BaseCapability)
			if kill != nil {
				return kill, nil
			}
			if hintText != "" {
				st.outputStagnationHinted = true
				st.stagnationHintAt = st.consecutiveRepeatCount
				st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Text: hintText})
			}
		} else {
			st.lastResultHash = 0
			st.consecutiveRepeatCount = 0
		}
	}

	st.stickyContext, toolResults = s.accumulateTurnObservations(req, resp, st.stickyContext, tcState.stickyResults, toolResults)
	vdaSt := &vdaState{turnAttachments: turnAttachments, toolResults: toolResults}
	s.runForcedVisualAudit(ctx, req, st.bindings, needsVerification, vdaSt)
	turnAttachments, toolResults = vdaSt.turnAttachments, vdaSt.toolResults
	s.persistAttachments(req.TaskID, req.StepID, turnAttachments)

	if turnDecisionID != "" && req.Hub != nil && req.Hub.Graph != nil {
		req.Hub.Graph.UpdateDecisionOutcome(turnDecisionID, fmt.Sprintf("executed %d tools", len(resp.ToolCalls)))
	}
	if turn == 0 {
		st.bindings = s.rerouteTurnZero(ctx, req, resp, prep.initialBindings, turn, prep.progressiveEnabled)
	}
	st.bindings = s.stickyUnionBindings(st.bindings, prep.initialBindings, st.calledTools)
	st.userBlocks = s.appendTurnObservation(st.userBlocks, resp, toolResults, needsVerification)

	newStreak, stallErr := s.fsOnlyStallGuard(ctx, req, role, resp, st.trace,
		st.fsOnlyTurnStreak, st.fsOnlyStreakLimit, turn, maxTurns, toolResults,
		st.effectiveProviderID, st.effectiveModelID)
	st.fsOnlyTurnStreak = newStreak
	return nil, stallErr
}
