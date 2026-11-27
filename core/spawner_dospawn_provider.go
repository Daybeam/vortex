package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/gateway"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

type spawnTurnAction int

const (
	spawnTurnSuccess spawnTurnAction = iota
	spawnTurnNextCandidate
	spawnTurnRetry
)

// spawnCandidateAttempt is one provider call's local working set. The caller
// commits userBlocks and the sanitized context even when the attempt fails,
// matching the original candidate loop's cross-candidate persistence.
type spawnCandidateAttempt struct {
	ctx           context.Context
	resp          *providers.ProviderResponse
	pCfg          *config.ProviderConfig
	systemBlocks  []schemas.ContentBlock
	userBlocks    []schemas.ContentBlock
	mcpServers    []providers.MCPServerDef
	attachments   []schemas.Attachment
	provider      providers.Provider
	bindings      []config.MCPBinding
	streamBuffer  strings.Builder
	temp          *float32
	freqPen       *float32
	forceTool     bool
	constraints   map[string]any
	early         *SpawnResult
	terminalErr   error
	skipCandidate bool
	err           error
}

// completeSpawnTurn preserves the candidate fallback and empty-response retry
// state machine. A retry restarts the outer turn; a candidate skip does not.
func (s *Spawner) completeSpawnTurn(ctx *context.Context, req *SpawnRequest, role *config.Role, prep *spawnPreparation, st *spawnLoopState, attachments []schemas.Attachment, turn, maxTurns int) (*providers.ProviderResponse, *config.ProviderConfig, *SpawnResult, spawnTurnAction, error) {
	const maxEmptyRespRetries = 2
	var resp *providers.ProviderResponse
	var currentPCfg *config.ProviderConfig

	for candIdx := range prep.candidateConfigs {
		attempt := s.streamSpawnCandidate(ctx, req, role, prep, st, attachments, candIdx, turn, maxTurns)
		*ctx = attempt.ctx
		st.userBlocks = attempt.userBlocks
		currentPCfg = attempt.pCfg
		if attempt.skipCandidate {
			continue
		}
		if attempt.terminalErr != nil {
			return nil, currentPCfg, nil, spawnTurnSuccess, attempt.terminalErr
		}
		if attempt.early != nil {
			return nil, currentPCfg, attempt.early, spawnTurnSuccess, nil
		}

		action, result, err := s.resolveSpawnCandidate(req, role, st, turn, &attempt, prep.candidateConfigs, candIdx, maxEmptyRespRetries)
		st.userBlocks = attempt.userBlocks
		if result != nil || err != nil {
			return nil, currentPCfg, result, spawnTurnSuccess, err
		}
		if action == spawnTurnRetry {
			return nil, currentPCfg, nil, action, nil
		}
		if action == spawnTurnNextCandidate {
			continue
		}
		resp = attempt.resp
		st.effectiveProviderID = currentPCfg.PoolID
		st.effectiveModelID = currentPCfg.Model
		break
	}
	return resp, currentPCfg, nil, spawnTurnSuccess, nil
}

// streamSpawnCandidate performs routing, prompt/Sieve preparation and exactly
// one StreamComplete call (plus provider telemetry). Retry policy lives in
// resolveSpawnCandidate so the network path remains linear.
func (s *Spawner) streamSpawnCandidate(ctx *context.Context, req *SpawnRequest, role *config.Role, prep *spawnPreparation, st *spawnLoopState, attachments []schemas.Attachment, candIdx, turn, maxTurns int) spawnCandidateAttempt {
	attempt := spawnCandidateAttempt{ctx: *ctx, userBlocks: st.userBlocks, attachments: attachments, bindings: st.bindings}
	cand := prep.candidateConfigs[candIdx]
	pCfg := s.resolveCandidateConfig(req.Hub, role, cand.providerID, cand.model)
	attempt.pCfg = pCfg

	if !pCfg.IsWithinValidity(time.Now()) && candIdx < len(prep.candidateConfigs)-1 {
		s.logger.Log(obs.EventProviderCooldownSkip, req.TaskID, req.StepID, map[string]any{
			"pool_id": providers.PoolIDFor(pCfg), "reason": "key_validity_expired",
		})
		attempt.skipCandidate = true
		return attempt
	}

	// Cooldown is best effort: skip only when another candidate exists.
	if req.Hub != nil && req.Hub.Registry != nil {
		pid := providers.PoolIDFor(pCfg)
		if cooling, remaining := req.Hub.Registry.IsCoolingDown(pid); cooling && candIdx < len(prep.candidateConfigs)-1 {
			s.logger.Log(obs.EventProviderCooldownSkip, req.TaskID, req.StepID, map[string]any{"pool_id": pid, "remaining_sec": remaining.Seconds(), "candidate_idx": candIdx})
			attempt.skipCandidate = true
			return attempt
		} else if req.Hub.Registry.CheckRecovery(pid) {
			s.logger.Log(obs.EventProviderRecovered, req.TaskID, req.StepID, map[string]any{"pool_id": pid})
		}
	}

	var provider providers.Provider
	slot, err := s.routeProvider(providers.PoolIDFor(pCfg), role.BaseCapability)
	if err == nil {
		provider = slot.Provider
	} else {
		provider, err = providers.Get(pCfg, req.Hub.Registry.ExternalRuntimes)
		if err != nil {
			if candIdx < len(prep.candidateConfigs)-1 {
				attempt.skipCandidate = true
				return attempt
			}
			attempt.err = err
			attempt.terminalErr = err
			return attempt
		}
	}

	if s.sanitizer != nil {
		attempt.ctx = gateway.ContextWithTaskID(attempt.ctx, req.TaskID)
		provider = gateway.NewSanitizingProvider(provider, s.sanitizer)
	}
	attempt.provider = provider

	// Dynamic Protocol: re-build prompts and servers for each candidate family.
	attempt.systemBlocks, err = s.buildSystemPrompt(attempt.ctx, req.Hub, role, prep.prunedSkills, role.BaseCapability, pCfg, prep.toolConstraints, prep.mergedContext, prep.toolFewShots, req.Isolation, prep.precedents, req.Task, "", req.TaskID, req.StepID, st.trace)
	if err != nil {
		attempt.early = s.failedOutput(role.BaseCapability, fmt.Sprintf("failed to render prompt: %v", err))
		return attempt
	}
	attempt.mcpServers = s.buildMCPServers(attempt.ctx, req.Hub, req.TaskID, st.bindings, pCfg.Provider, req.RoleID, req.OwnerID)
	if req.SharedContext != nil {
		attempt.mcpServers = append(attempt.mcpServers, sharedContextServerDef())
	}
	st.lastMCPServers = attempt.mcpServers
	attempt.userBlocks = s.maybeAppendDisclosureHint(req.Hub, prep.initialBindings, st.bindings, attempt.userBlocks, turn, prep.progressiveEnabled)

	attempt.temp = pCfg.Temperature
	if req.Temperature != nil {
		attempt.temp = req.Temperature
	}
	attempt.freqPen = pCfg.FrequencyPenalty
	if req.FrequencyPenalty != nil {
		attempt.freqPen = req.FrequencyPenalty
	}
	attempt.forceTool, attempt.constraints = s.resolveOutputConstraints(pCfg, turn, maxTurns, len(st.trace))

	var patternWindow []byte
	ctxWithTimeout, cancel := context.WithTimeout(attempt.ctx, 180*time.Second)
	s.logger.Log(obs.EventProviderCallStarted, req.TaskID, req.StepID, map[string]any{
		"turn": turn, "provider": pCfg.Provider, "model": pCfg.Model,
		"mcp_servers_count": len(attempt.mcpServers), "constraints": attempt.constraints, "candidate_idx": candIdx,
	})
	if s.registry.System.FeatureFlags.Debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] Calling Provider %s for Task %s Turn %d (candidate %d)\n", pCfg.Model, req.TaskID, turn, candIdx)
		_sysLen, _usrLen := 0, 0
		for _, b := range attempt.systemBlocks {
			_sysLen += len(b.Text)
		}
		for _, b := range attempt.userBlocks {
			_usrLen += len(b.Text)
		}
		fmt.Fprintf(os.Stderr, "[DEBUG][PROBE] task=%s turn=%d systemBlocks=%d(%dB) userBlocks=%d(%dB) sysFlat=%dB usrFlat=%dB tools=%d\n",
			req.TaskID, turn, len(attempt.systemBlocks), _sysLen, len(attempt.userBlocks), _usrLen,
			len(flattenContentBlocks(attempt.systemBlocks)), len(flattenContentBlocks(attempt.userBlocks)), len(attempt.mcpServers))
	}

	// Sieve Context Management (SOP 3.0).
	reqSieve := &schemas.CompleteRequest{
		SystemBlocks: attempt.systemBlocks, UserBlocks: attempt.userBlocks, Model: pCfg.Model,
		MaxTokens: s.spawnOutputBudget(req), CompressionHint: req.CompressionHint,
	}
	auditRes, err := s.contextManager.Audit(ctxWithTimeout, provider, reqSieve)
	if err == nil && auditRes.Decision != LevelNone {
		squeezed, squeezeRes, sErr := s.contextManager.Squeeze(ctxWithTimeout, provider, reqSieve, auditRes.Decision)
		if sErr == nil {
			// Squeeze populates flat strings; preserve blocks or rebuild them.
			if len(squeezed.SystemBlocks) > 0 || len(squeezed.UserBlocks) > 0 {
				attempt.systemBlocks = squeezed.SystemBlocks
				attempt.userBlocks = squeezed.UserBlocks
			} else if squeezed.System != "" || squeezed.User != "" {
				if squeezed.System != "" {
					attempt.systemBlocks = []schemas.ContentBlock{{Type: "text", Text: squeezed.System}}
				}
				if squeezed.User != "" {
					attempt.userBlocks = []schemas.ContentBlock{{Type: "text", Text: squeezed.User}}
				}
			}
			if squeezeRes.Decision == LevelCritical {
				cancel()
				attempt.err = fmt.Errorf("DYNAMIC_ESCALATION_REQUIRED: %s", squeezeRes.ActionTaken)
				attempt.terminalErr = attempt.err
				return attempt
			}
		}
	}

	providerStart := time.Now()
	attempt.resp, err = provider.StreamComplete(ctxWithTimeout, providers.CompleteRequest{
		SystemBlocks: attempt.systemBlocks, UserBlocks: prependSticky(attempt.userBlocks, st.stickyContext),
		System: flattenContentBlocks(attempt.systemBlocks), User: flattenContentBlocks(attempt.userBlocks),
		Model: pCfg.Model, MaxTokens: s.spawnOutputBudget(req), Temperature: attempt.temp,
		FrequencyPenalty: attempt.freqPen, MCPServers: attempt.mcpServers,
		Attachments: attempt.attachments, Secrets: req.Hub.Registry.Secrets,
		ForceToolCall: attempt.forceTool, Constraints: attempt.constraints,
	}, func(chunk string) error {
		attempt.streamBuffer.WriteString(chunk)
		patternWindow = append(patternWindow, chunk...)
		if len(patternWindow) > 150 {
			patternWindow = patternWindow[len(patternWindow)-150:]
		}
		if len(patternWindow) >= 150 {
			windowStr := string(patternWindow)
			tail := windowStr[len(windowStr)-30:]
			if strings.Count(windowStr, tail) >= 4 {
				return fmt.Errorf("ErrPatternLoopBrake")
			}
		}
		return nil
	})
	cancel()
	if s.providerRouter != nil {
		s.providerRouter.RecordResult(pCfg.Model, time.Since(providerStart), err)
	}
	attempt.err = err
	return attempt
}

func (a *spawnCandidateAttempt) request(req *SpawnRequest, st *spawnLoopState, maxTokens int) providers.CompleteRequest {
	return providers.CompleteRequest{
		SystemBlocks: a.systemBlocks, UserBlocks: prependSticky(a.userBlocks, st.stickyContext),
		System: flattenContentBlocks(a.systemBlocks), User: flattenContentBlocks(a.userBlocks),
		Model: a.pCfg.Model, MaxTokens: maxTokens, Temperature: a.temp, FrequencyPenalty: a.freqPen,
		MCPServers: a.mcpServers, Attachments: a.attachments, Secrets: req.Hub.Registry.Secrets,
		ForceToolCall: a.forceTool, Constraints: a.constraints,
	}
}

// resolveSpawnCandidate applies delegation, reactive compression, empty-response
// and retryable-error policy in the same order as the original turn loop.
func (s *Spawner) resolveSpawnCandidate(req *SpawnRequest, role *config.Role, st *spawnLoopState, turn int, attempt *spawnCandidateAttempt, candidates []spawnCandidate, candIdx, maxEmptyRespRetries int) (spawnTurnAction, *SpawnResult, error) {
	candidateCount := len(candidates)
	pCfg := attempt.pCfg
	err := attempt.err
	if err != nil && strings.Contains(err.Error(), "DELEGATION_REQUIRED") {
		attempt.early = &SpawnResult{Output: schemas.SubagentOutput{
			Status: schemas.StatusDelegationRequired, Confidence: 1.0,
			Result: map[string]any{
				"system_prompt": flattenContentBlocks(attempt.systemBlocks), "user_prompt": flattenContentBlocks(attempt.userBlocks),
				"tool_definitions": s.collectDelegationToolDefs(req.Hub, attempt.bindings),
			},
			Capability: role.BaseCapability,
		}}
		return spawnTurnSuccess, attempt.early, nil
	}

	if err != nil && IsContextLimitError(err) {
		budget := s.contextInputBudget(pCfg, req)
		cres := compressUserBlocksToBudget(attempt.userBlocks, budget)
		if cres.Changed {
			observability.GetGlobalMetrics().Inc("orchestrator.context.compressed", 1)
			s.logger.Log(obs.EventContextCompressed, req.TaskID, req.StepID, map[string]any{
				"turn": turn, "provider": pCfg.Provider, "model": pCfg.Model,
				"blocks_before": cres.Before, "blocks_after": cres.After, "dropped": cres.Dropped,
				"tokens_before": cres.BeforeTok, "tokens_after": cres.AfterTok, "budget": budget, "error": err.Error(),
			})
			attempt.userBlocks = cres.Blocks
			retryCtx, retryCancel := context.WithTimeout(attempt.ctx, 120*time.Second)
			attempt.resp, err = attempt.provider.StreamComplete(retryCtx, attempt.request(req, st, s.spawnOutputBudget(req)), func(chunk string) error {
				attempt.streamBuffer.WriteString(chunk)
				return nil
			})
			retryCancel()
			if err == nil {
				observability.GetGlobalMetrics().Inc("orchestrator.context.compress_retry_success", 1)
				s.logger.Log(obs.EventContextCompressRetrySuccess, req.TaskID, req.StepID, map[string]any{
					"turn": turn, "provider": pCfg.Provider, "model": pCfg.Model,
				})
				st.effectiveProviderID = pCfg.PoolID
				st.effectiveModelID = pCfg.Model
				attempt.err = nil
				return spawnTurnSuccess, nil, nil
			}
			s.logger.Log(obs.EventContextCompressRetryFailed, req.TaskID, req.StepID, map[string]any{
				"turn": turn, "provider": pCfg.Provider, "model": pCfg.Model, "error": err.Error(),
			})
		} else {
			s.logger.Log(obs.EventContextCompressSkipped, req.TaskID, req.StepID, map[string]any{
				"turn": turn, "provider": pCfg.Provider, "model": pCfg.Model, "reason": cres.Reason,
				"blocks": cres.Before, "tokens": cres.BeforeTok, "budget": budget, "error": err.Error(),
			})
		}
		attempt.err = err
	}

	if err == nil {
		resp := attempt.resp
		if resp.CompletionTokens == 0 && len(resp.ToolCalls) == 0 && strings.TrimSpace(resp.Text) == "" {
			s.logger.Log(obs.EventProviderFallback, req.TaskID, req.StepID, map[string]any{
				"turn": turn, "failed_provider": pCfg.Provider, "failed_model": pCfg.Model,
				"error": "empty_response_zero_tokens", "prompt_tokens": resp.PromptTokens,
				"next_candidate": func() string {
					if candIdx < candidateCount-1 {
						return candidates[candIdx+1].providerID
					}
					return ""
				}(),
			})
			if candIdx < candidateCount-1 {
				return spawnTurnNextCandidate, nil, nil
			}
			retry, exhaustErr := shouldRetryEmptyResponse(&st.emptyRespRetries, maxEmptyRespRetries)
			if retry {
				s.logger.Log(obs.EventProviderFallback, req.TaskID, req.StepID, map[string]any{
					"error": "empty_response_zero_tokens", "retry": st.emptyRespRetries, "max": maxEmptyRespRetries,
				})
				return spawnTurnRetry, nil, nil
			}
			s.logger.Log(obs.EventStepFailed, req.TaskID, req.StepID, map[string]any{
				"error":   "empty_response_zero_tokens",
				"details": fmt.Sprintf("all candidates returned 0 completion tokens after %d retries; possible causes: vLLM missing --tool-call-parser, KV cache overflow, safety filter, provider intermittent empty response", maxEmptyRespRetries),
			})
			s.persistTraceOnError(attempt.ctx, req, st.trace, st.effectiveProviderID, st.effectiveModelID, role.BaseCapability)
			return spawnTurnSuccess, nil, exhaustErr
		}
		return spawnTurnSuccess, nil, nil
	}

	if isRetryableError(err) && candIdx < candidateCount-1 {
		s.logger.Log(obs.EventProviderFallback, req.TaskID, req.StepID, map[string]any{
			"turn": turn, "failed_provider": pCfg.Provider, "failed_model": pCfg.Model,
			"error": err.Error(), "next_candidate": candidates[candIdx+1].providerID,
		})
		if s.expStore != nil {
			if repairs := s.expStore.QuerySkillsBySignal(attempt.ctx, err.Error()); len(repairs) > 0 {
				advice := fmt.Sprintf("%s: Encountered %q. Consider using these previously successful repair skills: %v", MarkerSystemAdvice, err.Error(), repairs)
				attempt.userBlocks = append(attempt.userBlocks, schemas.ContentBlock{Text: advice, CacheControl: "ephemeral"})
			}
		}
		return spawnTurnNextCandidate, nil, nil
	}

	s.logger.Log(obs.EventStepFailed, req.TaskID, req.StepID, map[string]any{
		"error": "Provider call failed (all candidates exhausted)", "details": err.Error(),
	})
	s.persistTraceOnError(attempt.ctx, req, st.trace, st.effectiveProviderID, st.effectiveModelID, role.BaseCapability)
	return spawnTurnSuccess, nil, err
}
