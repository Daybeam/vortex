package core

import (
	"context"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// tryDelegationMode handles the Subagent Delegation Mode (Zero-Config Cold
// Start, ADDED 2026-08-19). If delegation mode is enabled in the system
// config, it builds a delegation-required SpawnResult and returns it with
// handled=true. Otherwise returns (nil, false) and the caller continues
// with the normal tool execution loop.
// Extracted from spawner.go doSpawn during god-class split.
func (s *Spawner) tryDelegationMode(ctx context.Context, req *SpawnRequest, role *config.Role, prunedSkills []string, toolConstraints []string, mergedContext map[string]any, toolFewShots []string, userText string, bindings []config.MCPBinding) (*SpawnResult, bool) {
	if !s.registry.System.DelegationMode {
		return nil, false
	}
	pCfg := s.resolveCandidateConfig(req.Hub, role, req.ProviderOverride, "")
	// Defect 3 fix: redact sensitive context in delegation mode
	delegationCtx := redactContextForDelegation(mergedContext)
	delegationPrecedents := []*schemas.DecisionNode{}
	compileStart := time.Now()
	systemBlocks, err := s.buildSystemPrompt(ctx, req.Hub, role, prunedSkills, role.BaseCapability, pCfg, toolConstraints, delegationCtx, toolFewShots, req.Isolation, delegationPrecedents, req.Task, "", req.TaskID, req.StepID, nil)
	if err != nil {
		return nil, false
	}
	s.logger.Log(obs.EventStepStarted, req.TaskID, req.StepID, map[string]any{
		"action":      "delegation_compile_timing",
		"duration_ms": time.Since(compileStart).Milliseconds(),
	})
	s.logger.Log(obs.EventStepStarted, req.TaskID, req.StepID, map[string]any{
		"action": "delegation_triggered",
		"reason": "system_delegation_mode_enabled",
	})
	toolDefs := s.collectDelegationToolDefs(req.Hub, bindings)
	var delegatedStep *schemas.Step
	if req.Hub != nil && req.Hub.Graph != nil {
		delegatedStep = req.Hub.Graph.Steps[req.StepID]
	}
	return &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusDelegationRequired,
			Confidence: 1.0,
			Result: map[string]any{
				"system_prompt":     flattenContentBlocks(systemBlocks),
				"user_prompt":       userText,
				"target_capability": role.BaseCapability,
				"role_id":           req.RoleID,
				"tool_definitions":  toolDefs,
				"workspace":         delegationWorkspaceBlock(s.outputBase, req.TaskID, req.SessionRoot, delegatedStep),
			},
			Capability: role.BaseCapability,
		},
	}, true
}
