package core

import (
	"fmt"
	"strings"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// handleFulfillCapabilityRequired replaces the consumed delegation decision
// atomically. Calling addDecision directly would either deadlock under the
// caller's lock or expose a zero-decision window that can wake the run loop.
func (s *DirectedEngine) handleFulfillCapabilityRequired(taskID, decisionID string, output *schemas.SubagentOutput) error {
	s.Mu.Lock()
	graph := s.getOrReloadGraphLocked(taskID)
	if graph == nil {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found", taskID)
	}
	dec, _ := findPendingDecisionLocked(graph, decisionID)
	if dec == nil {
		s.Mu.Unlock()
		return fmt.Errorf("decision %q not found", decisionID)
	}
	if dec.Type != schemas.DecisionDelegationRequired {
		s.Mu.Unlock()
		return misroutedFulfillError(decisionID, dec.Type)
	}
	step := graph.Steps[dec.StepID]
	if step == nil {
		s.Mu.Unlock()
		return fmt.Errorf("step %q not found", dec.StepID)
	}
	roleID := step.RoleID
	submitterIsAdmin := graph.SubmitterIsAdmin
	s.Mu.Unlock()

	caps := normalizeRequiredCapabilities(output.RequiredCapabilities)
	options := s.capabilityDecisionOptions(roleID, caps)
	options = append(options, "rewrite_dag")
	ctx := fulfillCapabilityContext(submitterIsAdmin, roleID, output, caps)
	if s.registry != nil {
		ctx["available_models"] = s.registry.GetHealthyModelsSummary()
	}

	s.Mu.Lock()
	graph = s.getOrReloadGraphLocked(taskID)
	if graph == nil {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found", taskID)
	}
	if isTerminalStatus(graph.Status) {
		s.Mu.Unlock()
		return fmt.Errorf("task %q is already %q", taskID, graph.Status)
	}
	dec, decIdx := findPendingDecisionLocked(graph, decisionID)
	if dec == nil {
		s.Mu.Unlock()
		return fmt.Errorf("decision %q was removed by another operation", decisionID)
	}
	if dec.Type != schemas.DecisionDelegationRequired {
		s.Mu.Unlock()
		return misroutedFulfillError(decisionID, dec.Type)
	}
	step = graph.Steps[dec.StepID]
	if step == nil {
		s.Mu.Unlock()
		return fmt.Errorf("step %q not found", dec.StepID)
	}

	maxSkips := 3
	if s.registry != nil && s.registry.System.MaxConsecutiveSkips > 0 {
		maxSkips = s.registry.System.MaxConsecutiveSkips
	}
	options = filterOptionsForTier(graph.ConsecutiveSkips, maxSkips, ctx, options, graph.SubmitterIsAdmin)
	replacement := &schemas.Decision{
		ID:      schemas.NewDecisionID(),
		StepID:  step.ID,
		Type:    schemas.DecisionStepFailed,
		Context: ctx,
		Options: options,
		Created: time.Now(),
	}
	graph.PendingDecisions[decIdx] = replacement
	step.Status = schemas.StepBlocked
	step.LastError = "capability_required: " + strings.Join(caps, ", ")
	if step.LastError == "capability_required: " {
		step.LastError = "capability_required: host reported missing capabilities"
	}
	step.LastWorkedOn = time.Now()
	s.Mu.Unlock()

	s.persistGraph(graph)
	if s.auditLogger != nil {
		s.auditLogger.Log("addDecision", taskID, step.ID, replacement.ID, string(replacement.Type), "", options)
	}
	s.logger.Log(obs.EventDecisionRequired, taskID, step.ID, map[string]any{
		"decision_id": replacement.ID,
		"type":        replacement.Type,
		"options":     options,
	})
	s.publishDecisionEvent(graph, step, replacement, replacement.Type, ctx, options)
	s.broadcastDone(taskID)
	s.maybeAutoResolveNonInteractive(graph, step, replacement, replacement.Type, ctx, options)
	s.Broadcast()
	return nil
}

func (s *DirectedEngine) capabilityDecisionOptions(roleID string, details []string) []string {
	options := []string{"skip", "abort", "retry", "refine_and_retry"}
	if s.expStore != nil {
		for _, cap := range details {
			recommended := s.expStore.QuerySkillRecommendations(s.lifecycleCtx, cap, 0.0)
			if len(recommended) > 0 {
				best := s.expStore.SelectSkill(s.lifecycleCtx, recommended, cap, roleID, "", 0.1)
				if best != "" {
					options = append(options, "retry_with_skill:"+best)
				}
			}
			options = append(options, "retry_with_skill:"+cap)
		}
	} else {
		for _, cap := range details {
			options = append(options, "retry_with_skill:"+cap)
		}
	}
	return options
}

func fulfillCapabilityContext(submitterIsAdmin bool, roleID string, output *schemas.SubagentOutput, caps []string) map[string]any {
	scope := "self"
	if submitterIsAdmin {
		scope = "pool"
	}
	query := "<capability>"
	if len(caps) > 0 {
		query = caps[0]
	}
	advisory := map[string]any{
		"required_capabilities": caps,
		"discover": fmt.Sprintf(
			`orchestrator_invoke(subsystem="discovery", action="discover", scope=%q, type="tools", query=%q)`,
			scope, query,
		),
		"retry_with_skill": "Secondary only: reissues a delegation envelope; it does not mount a capability in the host environment.",
	}
	suggestion := "Discover tools available to your role, or configure the required capability in your own environment. proxy.call is admin-only."
	if submitterIsAdmin {
		advisory["invoke"] = `orchestrator_invoke(subsystem="proxy", action="call", tool=<name>, arguments=<args>)`
		suggestion = "Discover the missing tool in the pool, then call it through proxy.call. retry_with_skill is secondary and only reissues a delegation envelope."
	}
	ctx := map[string]any{
		"role_id":             roleID,
		"root_cause":          string(FailureClassCapabilityRequired),
		"suggested_action":    suggestion,
		"missing_details":     caps,
		"confidence":          output.Confidence,
		"capability_advisory": advisory,
	}
	return ctx
}

func normalizeRequiredCapabilities(caps []string) []string {
	out := make([]string, 0, len(caps))
	seen := make(map[string]struct{}, len(caps))
	for _, cap := range caps {
		cap = strings.TrimSpace(cap)
		if cap == "" {
			continue
		}
		if _, ok := seen[cap]; ok {
			continue
		}
		seen[cap] = struct{}{}
		out = append(out, cap)
	}
	return out
}
