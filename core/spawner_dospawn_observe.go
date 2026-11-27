package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// observeSpawnResponse records usage, truncation, outer-ring budget and cache
// telemetry. It is deliberately read-only with respect to execution flow.
func (s *Spawner) observeSpawnResponse(req *SpawnRequest, st *spawnLoopState, resp *providers.ProviderResponse, pCfg *config.ProviderConfig) {
	GetGlobalStats().RecordUsage(pCfg.Provider, pCfg.Model, resp.PromptTokens, resp.CompletionTokens, len(resp.ToolCalls), resp.CacheWriteTokens, resp.CacheReadTokens)

	if resp.StopReason == "length" || resp.StopReason == "max_tokens" {
		detail := map[string]any{
			"provider": pCfg.Provider, "model": pCfg.Model, "stop_reason": resp.StopReason,
			"completion_tokens": resp.CompletionTokens, "prompt_tokens": resp.PromptTokens,
			"text_chars": len(resp.Text), "tool_calls": len(resp.ToolCalls), "max_tokens": s.spawnOutputBudget(req),
		}
		if len(resp.Text) == 0 && len(resp.ToolCalls) == 0 && resp.CompletionTokens > 0 {
			detail["visible_output_empty"] = true
			detail["likely_reasoning_dominated"] = true
		}
		s.logger.Log(obs.EventOutputTruncated, req.TaskID, req.StepID, detail)
	}

	historyTokens := 0
	for i := range st.userBlocks {
		historyTokens += providers.EstimateTokens(st.userBlocks[i].Text)
	}
	s.UpdateSystemPromptOuterRing(estimateMCPServersTokens(st.lastMCPServers), historyTokens, resp.PromptTokens, resp.CompletionTokens)

	// Cost Governance: pure observer, never an execution gate.
	if alert, n := s.cacheSentinel.Observe(req.TaskID, req.StepID, resp.PromptTokens, resp.CacheReadTokens); alert {
		s.logger.Log(obs.EventCacheInefficiency, req.TaskID, req.StepID, map[string]any{
			"consecutive_zero_rounds": n, "prompt_tokens": resp.PromptTokens,
			"cache_read_tokens": resp.CacheReadTokens, "model": pCfg.Model,
			"possible_causes": []string{
				"gateway/SDK not forwarding usage.cache_read_input_tokens",
				"prompt prefix changes every turn (timestamp/random injection) breaking cache stability",
				"current model channel does not support prompt caching",
				"Anthropic cache_control markers misplaced",
			},
		})
	}
	if resp.PromptTokens > 0 {
		s.logger.Log("cache_hit_rate", req.TaskID, req.StepID, map[string]any{
			"prompt_tokens": resp.PromptTokens, "cache_read_tokens": resp.CacheReadTokens,
			"cache_hit_rate": float64(resp.CacheReadTokens) / float64(resp.PromptTokens), "model": pCfg.Model,
		})
	}
}

// captureSpawnDecision preserves the original thought logging, graph node,
// schema fingerprint and experience-precedent ordering.
func (s *Spawner) captureSpawnDecision(ctx context.Context, req *SpawnRequest, st *spawnLoopState, resp *providers.ProviderResponse, turn int) string {
	matches := thoughtRe.FindAllStringSubmatch(resp.Text, -1)
	if len(matches) == 0 {
		return ""
	}
	var thoughts []string
	var reasoning strings.Builder
	for _, m := range matches {
		thoughts = append(thoughts, strings.TrimSpace(m[1]))
		reasoning.WriteString(m[1])
		reasoning.WriteString("\n")
	}
	s.logger.Log("EventStepThought", req.TaskID, req.StepID, map[string]any{
		"thought": strings.Join(thoughts, "\n---\n"),
	})
	if req.Hub == nil || req.Hub.Graph == nil {
		return ""
	}

	turnDecisionID := fmt.Sprintf("dec_%s_%d", req.StepID, turn)
	node := &schemas.DecisionNode{
		ID: turnDecisionID, StepID: req.StepID, Turn: turn,
		Reasoning: strings.TrimSpace(reasoning.String()), Timestamp: time.Now(),
	}
	if len(resp.ToolCalls) > 0 {
		node.Type = "selection"
		var tools []string
		for _, tc := range resp.ToolCalls {
			tools = append(tools, tc.Name)
		}
		node.Action = fmt.Sprintf("call_tools: %v", tools)
	} else {
		node.Type = "synthesis"
		node.Action = "finalize_answer"
	}
	for k := range req.ContextRefs {
		node.Causes = append(node.Causes, k)
	}
	req.Hub.Graph.RecordDecision(turnDecisionID, node)
	st.decisionIDs = append(st.decisionIDs, turnDecisionID)
	if len(resp.ToolCalls) > 0 {
		node.SchemaHashes = s.collectCurrentSchemaFingerprints()
	}
	if s.expStore != nil {
		s.expStore.UpsertDecisionPrecedent(ctx, node)
	}
	return turnDecisionID
}

// handleSpawnHeartbeat returns a terminal result or asks the turn loop to
// continue after a sleep. Cancellation still interrupts heartbeat sleep.
func (s *Spawner) handleSpawnHeartbeat(ctx context.Context, req *SpawnRequest, st *spawnLoopState, resp *providers.ProviderResponse, capability string, turn int) (*SpawnResult, bool) {
	hb := parseHeartbeat(resp.Text)
	switch hb.Action {
	case HeartbeatComplete:
		s.logger.Log("EventHeartbeatComplete", req.TaskID, req.StepID, map[string]any{"turn": turn})
		return &SpawnResult{Output: schemas.SubagentOutput{
			Status: schemas.StatusOK, Confidence: 1.0, Result: map[string]any{"report": hb.Report}, Capability: capability,
		}, ProviderID: st.effectiveProviderID, ModelID: st.effectiveModelID, TurnsUsed: turn + 1}, false
	case HeartbeatFail:
		s.logger.Log("EventHeartbeatFail", req.TaskID, req.StepID, map[string]any{"turn": turn})
		return &SpawnResult{Output: schemas.SubagentOutput{
			Status: schemas.StatusFailed, Confidence: 0.1, Result: map[string]any{"error": hb.Report}, Capability: capability,
		}, ProviderID: st.effectiveProviderID, ModelID: st.effectiveModelID, TurnsUsed: turn + 1}, false
	case HeartbeatSleep:
		s.logger.Log("EventHeartbeatSleep", req.TaskID, req.StepID, map[string]any{"turn": turn, "sleep_seconds": hb.SleepSeconds})
		timer := time.NewTimer(time.Duration(hb.SleepSeconds) * time.Second)
		select {
		case <-timer.C:
			if hb.WakePrompt != "" {
				st.userBlocks = append(st.userBlocks, schemas.ContentBlock{Type: "text", Text: "[Heartbeat WakePrompt]\n" + hb.WakePrompt})
			}
			return nil, true
		case <-ctx.Done():
			timer.Stop()
			return s.failedOutput(capability, "heartbeat sleep interrupted: task cancelled"), false
		}
	default:
		return nil, false
	}
}
