package core

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// sieveTurnState holds the loop-carried fields the sieve-guardian block mutates.
type sieveTurnState struct {
	sieveGuardianHits int
	userBlocks        []schemas.ContentBlock
}

// sieveGuardianTurn runs the Sieve Guardian tool-loop detector for this turn
// and reports what the loop should do next.
//
// Extracted from doSpawn (size gate); behaviour is identical. Two-tier
// escalation: the first interception injects a correction prompt and skips the
// turn (skip); a repeat escalates — ask the provider for a best-effort answer
// and terminate with it, or terminate with the failed output.
func (s *Spawner) sieveGuardianTurn(
	ctx context.Context, req *SpawnRequest, role *config.Role,
	resp *providers.ProviderResponse, trace []store.ToolInteraction,
	turn int, currentPCfg *config.ProviderConfig,
	effectiveProviderID, effectiveModelID string, statesVisited []string,
	st *sieveTurnState,
) turnOutcome {
	valid, reason := s.checkSieveGuardian(req.TaskID, req.StepID, turn, resp.ToolCalls, trace)
	if valid {
		return turnOutcome{}
	}
	st.sieveGuardianHits++
	observability.GetGlobalMetrics().Inc("orchestrator.sieve.interceptions", 1)
	if st.sieveGuardianHits == 1 {
		st.userBlocks = append(st.userBlocks, schemas.ContentBlock{
			Text: fmt.Sprintf(SpawnerLoopDetectedFmt, reason),
		})
		return turnOutcome{skip: true}
	}
	if noToolProvider, perr := providers.Get(currentPCfg, req.Hub.Registry.ExternalRuntimes); perr == nil {
		noToolResp, nerr := noToolProvider.StreamComplete(ctx, providers.CompleteRequest{
			UserBlocks: append(st.userBlocks, schemas.ContentBlock{
				Text: SpawnerLoopPersistMsg,
			}),
			System:    SysPromptBestAnswer,
			Model:     currentPCfg.Model,
			MaxTokens: s.spawnOutputBudget(req),
		}, nil)
		if nerr == nil && noToolResp.Text != "" {
			degradedOutput := schemas.ParseOutput(noToolResp.Text, role.BaseCapability)
			degradedOutput.Status = schemas.StatusPartial
			if degradedOutput.Confidence > 0.3 {
				degradedOutput.Confidence = 0.3
			}
			degradedOutput.Warnings = append(degradedOutput.Warnings, "degraded: "+reason)
			return turnOutcome{done: true, result: &SpawnResult{
				Output:        degradedOutput,
				ProviderID:    effectiveProviderID,
				ModelID:       effectiveModelID,
				Ref:           req.TaskID + ":" + req.StepID,
				TurnsUsed:     turn + 2,
				StatesVisited: statesVisited,
			}}
		}
	}
	return turnOutcome{done: true, result: s.failedOutput(role.BaseCapability, reason)}
}
