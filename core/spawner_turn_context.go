package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// accumulateTurnObservations folds this turn's sticky (auth/login) results into
// stickyContext (dedup by substring containment) and appends a gateway-watchdog
// alert to toolResults when one fired.
//
// Extracted from doSpawn (size gate); no control-flow effect — it returns the
// updated values for the caller to store.
func (s *Spawner) accumulateTurnObservations(
	req *SpawnRequest, resp *providers.ProviderResponse,
	stickyContext string, stickyResults, toolResults []string,
) (string, []string) {
	stickyContext = accumulateSticky(stickyContext, stickyResults)

	// ── Priority 3: Gateway Watchdog (Active Conflict Probing) ─────────
	if alert := s.watchdog.Sniff(resp.ToolCalls, req.Hub); alert != nil {
		toolResults = append(toolResults, fmt.Sprintf("\n%s", alert.Message))
		s.logger.Log(EventSwarmWatchdogTriggered, req.TaskID, req.StepID, map[string]any{
			"type": alert.Type,
		})
	}
	return stickyContext, toolResults
}

// appendTurnObservation appends the assistant turn and the tool-observation
// block (observation + self-healing advice + any VDA prompt) to userBlocks.
//
// Rolling-Marker strategy: every prior block's cache marker is cleared before
// the new turn is appended. Extracted from doSpawn (size gate); no control-flow
// effect — it returns the new userBlocks slice.
func (s *Spawner) appendTurnObservation(
	userBlocks []schemas.ContentBlock, resp *providers.ProviderResponse,
	toolResults []string, needsVerification bool,
) []schemas.ContentBlock {
	vdaPrompt := ""
	if needsVerification {
		vdaPrompt = SpawnerVDAPrompt
	}

	for i := range userBlocks {
		userBlocks[i].CacheControl = ""
	}
	userBlocks = append(userBlocks, schemas.ContentBlock{Text: resp.Text}) // Assistant turn

	// Mechanism 1: Single-Action Observation Loop
	// Mechanism 2: Error-Driven Self-Healing Classifier
	observation := strings.Join(toolResults, "\n")
	healingAdvice := s.healer.Diagnose(observation)

	resMD := fmt.Sprintf("%s\n%s%s%s", MarkerEnvObservation, observation, healingAdvice, vdaPrompt)
	userBlocks = append(userBlocks, schemas.ContentBlock{Text: resMD, CacheControl: "ephemeral"}) // Tool results turn

	// VCP Context Tiering §3 Bucket 2: compact old tool observations into
	// one-line summaries to prevent context pollution in long-running
	// heartbeat tasks. Only the latest MaxFullObservations are kept in full.
	return CompactOldObservations(userBlocks, MaxFullObservations)
}
