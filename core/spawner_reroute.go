package core

import (
	"context"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
)

// rerouteTurnZero re-routes the tool bindings on the FIRST turn based on the
// model's opening text (Progressive Disclosure). Extracted from doSpawn (size
// gate).
//
// Called only when turn == 0: re-routing on later turns would change the tool
// block and miss the prompt cache every turn. Later turns keep their bindings
// and grow through the sticky union (append-only surface); progressive
// discovery there is handled by the decision gate's refine_and_retry.
func (s *Spawner) rerouteTurnZero(
	ctx context.Context, req *SpawnRequest, resp *providers.ProviderResponse,
	initialBindings []config.MCPBinding, turn int, progressiveEnabled bool,
) []config.MCPBinding {
	return s.toolRouter.Route(ctx, RouteRequest{
		Task:               req.Task + " " + resp.Text,
		Bindings:           initialBindings,
		Turn:               turn + 1,
		ProgressiveMode:    progressiveEnabled,
		PredecessorSkillID: req.PredecessorSkillID,
		MaxTokenBudget:     s.toolSchemaBudget(),
		TaskID:             req.TaskID,
	})
}
