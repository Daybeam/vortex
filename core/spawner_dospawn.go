package core

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

func (s *Spawner) doSpawn(ctx context.Context, req *SpawnRequest) (*SpawnResult, error) {
	prep, earlyResult, err := s.prepareSpawn(ctx, req)
	if err != nil {
		return nil, err
	}
	if earlyResult != nil {
		return earlyResult, nil
	}

	st := newSpawnLoopState(s, prep.userBlocks)
	st.bindings = prep.bindings
	maxTurns := s.calculateMaxTurns(ctx, req, prep.role)
	const maxConsecutiveParseFailures = 3

turnLoop:
	for turn := 0; turn < maxTurns; turn++ {
		if ctx.Err() != nil {
			return s.failedOutput(prep.role.BaseCapability, fmt.Sprintf("context cancelled: %v", ctx.Err())), nil
		}

		turnCtx := NewTurnIdempotencyContext()
		attachments := s.resolveContextAttachments(ctx, req.TaskID, req.StepID)
		if len(attachments) > 0 {
			imgTokens := (&PromptBudgetEnforcer{}).EstimateImageTokens(attachments)
			s.logger.Log("EventImageTokensEstimated", req.TaskID, req.StepID, map[string]any{
				"attachment_count": len(attachments), "estimated_tokens": imgTokens,
			})
		}

		resp, currentPCfg, result, action, err := s.completeSpawnTurn(&ctx, req, prep.role, prep, st, attachments, turn, maxTurns)
		if result != nil {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if action == spawnTurnRetry {
			continue turnLoop
		}

		s.observeSpawnResponse(req, st, resp, currentPCfg)
		turnDecisionID := s.captureSpawnDecision(ctx, req, st, resp, turn)
		if result, skip := s.handleSpawnHeartbeat(ctx, req, st, resp, prep.role.BaseCapability, turn); result != nil || skip {
			if result != nil {
				return result, nil
			}
			continue
		}

		if len(resp.ToolCalls) == 0 {
			switch st.echoGuard.Check(resp.Text) {
			case EchoHint:
				s.logger.Log("EventOutputStagnationHinted", req.TaskID, req.StepID, map[string]any{
					"turn": turn, "signal": "text",
				})
				st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Text: EchoHintText})
			case EchoKill:
				s.logger.Log(obs.EventStepFailed, req.TaskID, req.StepID, map[string]any{
					"error":      "text_echo: identical response repeated after hint",
					"root_cause": "model_stuck", "signal": "text",
				})
				return s.partialEchoOutput(prep.role.BaseCapability,
					"text_echo: identical response repeated after intervention hint", st.trace), nil
			}

			turnState := &spawnTurnState{
				consecutiveParseFailures: st.consecutiveParseFailures,
				zeroEffectNudged:         st.zeroEffectNudged, midAbandonNudged: st.midAbandonNudged,
				userBlocks: st.userBlocks, trace: st.trace,
			}
			out := s.completeTurn(ctx, req, prep.role, resp, turnState, turn, maxTurns, turnDecisionID,
				st.effectiveProviderID, st.effectiveModelID, st.decisionIDs, st.statesVisited, maxConsecutiveParseFailures)
			st.consecutiveParseFailures = turnState.consecutiveParseFailures
			st.zeroEffectNudged = turnState.zeroEffectNudged
			st.midAbandonNudged = turnState.midAbandonNudged
			st.userBlocks = turnState.userBlocks
			if out.done {
				return out.result, nil
			}
			if out.skip {
				continue
			}
		}

		result, err = s.executeSpawnToolTurn(ctx, req, prep.role, prep, st, resp, currentPCfg, turn, maxTurns, turnCtx, turnDecisionID)
		if result != nil || err != nil {
			return result, err
		}
	}

	s.persistTraceOnError(ctx, req, st.trace, st.effectiveProviderID, st.effectiveModelID, prep.role.BaseCapability)
	return nil, fmt.Errorf("%s: tool execution loop reached max turns (%d) without a final answer; %d tool interaction(s) executed",
		ErrMaxTurnsExhausted, maxTurns, len(st.trace))
}
