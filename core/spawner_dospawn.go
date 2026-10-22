package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/gateway"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// defaultSpawnOutputTokens is the response cap for a spawn that does not ask
// for its own. It covers everything the model emits, and on a thinking model
// that includes reasoning tokens, which never reach response text.
//
// 8192, not the original 4096. Measured 2026-10-20 (gemini-3-flash-preview via
// an OpenAI-compatible gateway): three responses hit the cap at 4068/4069/3932
// completion tokens where the last emitted zero characters and zero tool calls
// — the whole budget was reasoning. The old value was sized for prose answers,
// which is the wrong unit once reasoning shares the same budget.
//
// Also used by the single-shot generation calls in this package that emit a
// structured document (role generation, reflection's Lua script), because they
// are subject to exactly the same failure: reasoning consumes the budget before
// the document starts. Keep one constant rather than three literals that drift.
const defaultSpawnOutputTokens = 8192

// defaultDedupHardCap is the reason-blind ceiling for identical calls inside
// one step, used when system.dedup_hard_cap is unset.
//
// Chosen at 5 because #11 already blocks the 3rd repeat for a model that does
// not explain itself; the cap only has to catch the model that *does*. Two
// extra calls is enough slack for a genuine retry (data refresh, transient
// error) without giving a looping model a budget worth burning.
const defaultDedupHardCap = 5

// dedupHardCap resolves the ceiling for one spawn, mirroring spawnOutputBudget:
// deployment setting first, built-in default second.
func (s *Spawner) dedupHardCap() int {
	if s != nil && s.registry != nil {
		s.registry.Mu.RLock()
		configured := s.registry.System.DedupHardCap
		s.registry.Mu.RUnlock()
		if configured > 0 {
			return configured
		}
	}
	return defaultDedupHardCap
}

// spawnOutputBudget resolves the response token cap for one spawn.
//
// Precedence: an explicit per-spawn request, then the deployment setting, then
// the built-in default. The middle step exists because the right cap depends on
// the model, not on this code: a thinking model needs room for reasoning that a
// prose-tuned constant cannot anticipate.
func (s *Spawner) spawnOutputBudget(req *SpawnRequest) int {
	if req != nil && req.MaxOutputTokens > 0 {
		return req.MaxOutputTokens
	}
	if s != nil && s.registry != nil {
		s.registry.Mu.RLock()
		configured := s.registry.System.DefaultSpawnOutputTokens
		s.registry.Mu.RUnlock()
		if configured > 0 {
			return configured
		}
	}
	return defaultSpawnOutputTokens
}

func (s *Spawner) doSpawn(ctx context.Context, req *SpawnRequest) (*SpawnResult, error) {
	prep, earlyResult, err := s.prepareSpawn(ctx, req)
	if err != nil {
		return nil, err
	}
	if earlyResult != nil {
		return earlyResult, nil
	}

	// Unpack preparation
	role := prep.role
	candidateConfigs := prep.candidateConfigs
	bindings := prep.bindings
	initialBindings := prep.initialBindings
	progressiveEnabled := prep.progressiveEnabled
	precedents := prep.precedents
	toolConstraints := prep.toolConstraints
	mergedContext := prep.mergedContext
	prunedSkills := prep.prunedSkills
	toolFewShots := prep.toolFewShots
	userBlocks := prep.userBlocks
	// == Tool Execution Loop (VDA Enhanced) ==================================

	var trace []store.ToolInteraction
	var decisionIDs []string
	var statesVisited []string // PGPO (ADDED 2026-09-08)
	var previousState string   // PGPO (ADDED 2026-09-08)
	toolFailCount := make(map[string]int)
	// #11 (eval §8.41): Per-task identical-call counter for dedup.
	// Key = toolName + ":" + fnv(args). Allows 2 identical calls; the 3rd
	// requires a _reason argument (legitimate retries like data refresh pass).
	// Per-step scope: different steps may legitimately make the same call
	// (e.g., parallel searches gathering information). Task-level scope would
	// block normal cross-step work.
	toolCallCount := make(map[string]int) // callKey → count (resets per step)
	// #11b: per-step streak of byte-identical answers for the same canonical
	// arguments. The hard cap blocks on this, not on the raw call count, so a
	// legitimate retry whose answer changes (token refresh) is never counted.
	resultStreak := make(map[string]int)
	resultLastHash := make(map[string]uint64)
	sieveGuardianHits := 0
	// Zero-effect completion gate (ADDED 2026-10-21, eval v14). Fires at most
	// once per step: the point is to make the model notice that nothing it did
	// could have produced the state it is claiming, not to fight it in a loop.
	// If the model answers "this was a read-only question", that is a legitimate
	// answer and the second pass goes through.
	zeroEffectNudged := false
	// A5 (eval FINDINGS.md §A5): mid-abandonment nudge — one-shot, like the
	// zero-effect nudge above it.
	midAbandonNudged := false
	// B2 (eval §3.2): Output hash dedup for execute_code loop detection.
	// C3 fix (2026-10-18): Consecutive-only tracking. The previous cumulative
	// map (resultHashCounts) produced false positives for legitimate polling
	// (e.g., check_status every 5 turns — same hash on turns 1,6,11,16 would
	// trigger stagnation even though the model was making progress between
	// polls). Now we track only consecutive repeats: if the hash changes,
	// the count resets to 1. Only maxConsecutiveDuplicates identical turns
	// in a row trigger intervention.
	//
	// The hash now includes tool call names + arguments (not just results)
	// so different calls with the same short output don't collide. Short
	// results (< MinResultBytesForDedup) and all-error turns are skipped
	// entirely — they legitimately repeat and are tracked elsewhere.
	var lastResultHash uint64
	var consecutiveRepeatCount int
	outputStagnationHinted := false
	const maxConsecutiveDuplicates = 3
	// P1 fix (2026-10-03, eval §0.5): Consecutive parse failure counter.
	// After 3 consecutive unparseable outputs, break the spin loop instead
	// of burning 19-43 provider calls on identical failures.
	consecutiveParseFailures := 0
	const maxConsecutiveParseFailures = 3

	// #15 (eval §8.35): No-progress turn detection. Tracks consecutive turns
	// where ALL tool calls were fs-only (write_file/read_file/execute_code)
	// with zero domain MCP calls. A high streak indicates the model is stuck
	// in a write→read self-proof loop burning the turn budget. Pure metrics —
	// no behavior change; threshold/action to be added after an arm validates.
	fsOnlyTurnStreak := 0
	// #4+#15 (eval §8.37.2/§8.40): Configurable fs-only streak limit.
	// When the model makes N consecutive turns using ONLY filesystem tools
	// (write_file/read_file/execute_code) without any domain MCP calls,
	// break the turn loop — the model is stuck in a self-proof loop.
	//
	// NECESSITY: Premature — no production evidence of this pattern. maxTurns
	// (default 50) + #11 dedup already bound the problem. The gap this catches
	// (different fs-only calls each turn, no domain progress) is narrow.
	// Design: measure-first-then-act. Deploy with metrics only (limit=0),
	// monitor orchestrator.progress.fs_only_streak_max, enable threshold only
	// if metrics justify it. See docs/completed/2026-10-03/ §#4+#15.
	//
	// Default 0 = disabled (metrics only). Set VORTEX_FS_ONLY_STREAK_LIMIT=5
	// to enable. Start conservative to avoid aborting legitimate long
	// reasoning chains that use fs-only tools productively.
	// Migrated to Registry.System.FeatureFlags (hot-reloadable).
	fsOnlyStreakLimit := s.registry.System.FeatureFlags.FSOnlyStreakLimit

	maxTurns := s.calculateMaxTurns(ctx, req, role)
	// applyStrategyBias is now handled inside calculateMaxTurns via PolicyDecider

	var effectiveProviderID string
	var effectiveModelID string

	// Empty response retry: retry the turn when the provider returns HTTP 200
	// but zero tokens (intermittent server-side empty response, e.g. Xunfei MaaS).
	// See eval §10.4: ~40% of Xunfei responses had non-zero usage but empty content.
	const maxEmptyRespRetries = 2
	emptyRespRetries := 0

	// Outer-ring observability (ADDED 2026-10-12): systemBlocks / mcpServers
	// are rebuilt per candidate inside the loops below; keep the last-built
	// MCP server list so the per-turn outer-ring accounting after the
	// candidate loop can price the tool schemas actually sent.
	var lastMCPServers []providers.MCPServerDef

	// stickyContext accumulates auth/login results across turns. Prepended to
	// UserBlocks before each StreamComplete so the model always has persistent
	// context near the top (AppWorld eval: prevent burial at position 5/51).
	var stickyContext string

	// R2: Sticky union — tools called in prior turns survive re-route.
	// Without this, re-routing at each turn can drop tools the model was
	// actively using (e.g. show_sent_payment_requests called in turn 1,
	// dropped in turn 2 because the task text changed), breaking multi-turn
	// workflows and invalidating prompt-cache prefixes.
	calledTools := make(map[string]bool)

turnLoop:
	for turn := 0; turn < maxTurns; turn++ {
		// Abort if the task context was cancelled (e.g. CancelTask or shutdown).
		if ctx.Err() != nil {
			return s.failedOutput(role.BaseCapability, fmt.Sprintf("context cancelled: %v", ctx.Err())), nil
		}

		// A1 (Effect Boundary Defense): per-turn idempotency context.
		// Duplicate tool calls within the same turn are skipped to prevent
		// repeated execution (A1 anomaly). Resets each turn — different
		// turns may legitimately repeat calls (e.g., after provider retry).
		// See docs/plan/EFFECT_BOUNDARY_DEFENSE_DESIGN.md §3.2.
		turnCtx := NewTurnIdempotencyContext()

		// VDA: Attach visual context from current state
		attachments := s.resolveContextAttachments(ctx, req.TaskID, req.StepID)

		// Log estimated image token cost so VLM attachments are visible in
		// observability. See docs/plan/VLM_IMAGE_CONTEXT_OPTIMIZATION_DESIGN.md §1b.
		if len(attachments) > 0 {
			imgTokens := (&PromptBudgetEnforcer{}).EstimateImageTokens(attachments)
			s.logger.Log("EventImageTokensEstimated", req.TaskID, req.StepID, map[string]any{
				"attachment_count": len(attachments),
				"estimated_tokens": imgTokens,
			})
		}

		var resp *providers.ProviderResponse
		var currentPCfg *config.ProviderConfig

		for candIdx, cand := range candidateConfigs {
			pCfg := s.resolveCandidateConfig(req.Hub, role, cand.providerID, cand.model)
			currentPCfg = pCfg

			// FIX (2026-08-16/17): the global rate-limit cooldown set by
			// scheduler.go's SetCooldown() was write-only -- nothing ever
			// consulted Registry.IsCoolingDown(), so a provider marked as
			// cooling down after a 429 was retried immediately anyway by any
			// concurrent step. Skip to the next candidate (if any) while this
			// one is cooling down; if it's the last/only candidate, fail open
			// and attempt it anyway rather than blocking the step entirely --
			// the cooldown is a best-effort optimization, not a hard gate.
			if req.Hub != nil && req.Hub.Registry != nil {
				if cooling, remaining := req.Hub.Registry.IsCoolingDown(providers.PoolIDFor(pCfg)); cooling && candIdx < len(candidateConfigs)-1 {
					s.logger.Log(EventProviderCooldownSkip, req.TaskID, req.StepID, map[string]any{
						"pool_id":       providers.PoolIDFor(pCfg),
						"remaining_sec": remaining.Seconds(),
						"candidate_idx": candIdx,
					})
					continue
				}
			}

			var provider providers.Provider

			slot, err := s.routeProvider(providers.PoolIDFor(pCfg), role.BaseCapability)
			if err == nil {
				provider = slot.Provider
			} else {
				provider, err = providers.Get(pCfg, req.Hub.Registry.ExternalRuntimes)
				if err != nil {
					if candIdx < len(candidateConfigs)-1 {
						continue
					}
					return nil, err
				}
			}

			if s.sanitizer != nil {
				ctx = gateway.ContextWithTaskID(ctx, req.TaskID)
				provider = gateway.NewSanitizingProvider(provider, s.sanitizer)
			}

			// Dynamic Protocol: Re-build prompts and servers for each candidate family
			systemBlocks, err := s.buildSystemPrompt(ctx, req.Hub, role, prunedSkills, role.BaseCapability, pCfg, toolConstraints, mergedContext, toolFewShots, req.Isolation, precedents, req.Task, "", req.TaskID)
			if err != nil {
				return s.failedOutput(role.BaseCapability, fmt.Sprintf("failed to render prompt: %v", err)), nil
			}
			mcpServers := s.buildMCPServers(ctx, req.Hub, req.TaskID, bindings, pCfg.Provider, req.RoleID)
			lastMCPServers = mcpServers

			// Priority 4: Dynamic Re-routing & Progressive Disclosure Hint
			if turn == 0 && progressiveEnabled {
				// Check if we actually hid many tools
				totalTools := 0
				for _, b := range initialBindings {
					m := req.Hub.GetMCP(b.MCPID)
					if m != nil {
						totalTools += len(m.AvailableTools)
					}
				}
				visibleTools := 0
				for _, b := range bindings {
					visibleTools += len(b.AllowedTools)
				}

				if totalTools > visibleTools+5 {
					hint := fmt.Sprintf("\n[SYSTEM HINT]: I have over %d specialized tools available but am currently exposing only the %d most relevant discovery/status tools to keep your context focused. Use discovery tools (list, search, status) to explore the environment; as your intent becomes more specific, I will automatically unlock the necessary specialized tools.", totalTools, visibleTools)
					userBlocks = append(userBlocks, schemas.ContentBlock{Text: hint, CacheControl: "ephemeral"})
				}
			}

			// Apply generation parameter overrides
			temp := pCfg.Temperature
			if req.Temperature != nil {
				temp = req.Temperature
			}
			freqPen := pCfg.FrequencyPenalty
			if req.FrequencyPenalty != nil {
				freqPen = req.FrequencyPenalty
			}

			// Apply Output Constraints
			// FIX (2026-09-07): Revert ForceToolCall to only turn-0 pressure.
			// ForceToolCall=ANY on Gemini is a hard constraint that forbids text
			// output — forcing it across the whole budget would trap the model
			// into calling tools it doesn't need (like `think` with no args) just
			// to satisfy the schema. The real fix is the re-prompt nudge below:
			// when the model writes text early, tell it to keep calling tools.
			// This preserves the model's ability to decide when it's done.
			constraints := make(map[string]any)
			forceTool := turn < maxTurns-1 && len(trace) == 0 && !s.registry.System.FeatureFlags.DisableForceTool
			if forceTool {
				toolCallSchema := map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":      map[string]any{"type": "string"},
							"arguments": map[string]any{"type": "object"},
						},
						"required": []string{"name", "arguments"},
					},
				}
				field, val := s.constraintAdapter.Resolve(pCfg.Provider, toolCallSchema)
				if field != "" {
					constraints[field] = val
				}
			}

			var streamBuffer strings.Builder
			var patternWindow []byte
			ctxWithTimeout, cancel := context.WithTimeout(ctx, 180*time.Second)

			s.logger.Log(EventProviderCallStarted, req.TaskID, req.StepID, map[string]any{
				"turn":              turn,
				"provider":          pCfg.Provider,
				"model":             pCfg.Model,
				"mcp_servers_count": len(mcpServers),
				"constraints":       constraints,
				"candidate_idx":     candIdx,
			})

			if s.registry.System.FeatureFlags.Debug {
				fmt.Fprintf(os.Stderr, "[DEBUG] Calling Provider %s for Task %s Turn %d (candidate %d)\n", pCfg.Model, req.TaskID, turn, candIdx)
				// v26 probe: prompt emptiness forensics
				_sysLen, _usrLen := 0, 0
				for _, b := range systemBlocks {
					_sysLen += len(b.Text)
				}
				for _, b := range userBlocks {
					_usrLen += len(b.Text)
				}
				fmt.Fprintf(os.Stderr, "[DEBUG][PROBE] task=%s turn=%d systemBlocks=%d(%dB) userBlocks=%d(%dB) sysFlat=%dB usrFlat=%dB tools=%d\n",
					req.TaskID, turn, len(systemBlocks), _sysLen, len(userBlocks), _usrLen,
					len(flattenContentBlocks(systemBlocks)), len(flattenContentBlocks(userBlocks)), len(mcpServers))
			}

			// --- Sieve Context Management (SOP 3.0) ---
			reqSieve := &schemas.CompleteRequest{
				SystemBlocks:    systemBlocks,
				UserBlocks:      userBlocks,
				Model:           pCfg.Model,
				CompressionHint: req.CompressionHint,
			}
			auditRes, err := s.contextManager.Audit(ctxWithTimeout, provider, reqSieve)
			if err == nil && auditRes.Decision != LevelNone {
				squeezed, squeezeRes, sErr := s.contextManager.Squeeze(ctxWithTimeout, provider, reqSieve, auditRes.Decision)
				if sErr == nil {
					// FIX (2026-10-12, Kaggle B-arm empty-prompt root cause):
					// Squeeze's returned CompleteRequest only populates the flat
					// System/User strings; SystemBlocks/UserBlocks are left nil.
					// Assigning them back unconditionally wiped the ENTIRE prompt
					// whenever Audit decided any non-None level (mock/llama.cpp
					// CountTokens returns garbage) — the model then received an
					// empty system+user and could only greet/clarify with zero
					// tool calls. Preserve content: prefer blocks, else rebuild
					// blocks from the compressed flat strings.
					if len(squeezed.SystemBlocks) > 0 || len(squeezed.UserBlocks) > 0 {
						systemBlocks = squeezed.SystemBlocks
						userBlocks = squeezed.UserBlocks
					} else if squeezed.System != "" || squeezed.User != "" {
						if squeezed.System != "" {
							systemBlocks = []schemas.ContentBlock{{Type: "text", Text: squeezed.System}}
						}
						if squeezed.User != "" {
							userBlocks = []schemas.ContentBlock{{Type: "text", Text: squeezed.User}}
						}
					}
					if squeezeRes.Decision == LevelCritical {
						cancel()
						return nil, fmt.Errorf("DYNAMIC_ESCALATION_REQUIRED: %s", squeezeRes.ActionTaken)
					}
				}
			}

			providerStart := time.Now()
			resp, err = provider.StreamComplete(ctxWithTimeout, providers.CompleteRequest{
				SystemBlocks:     systemBlocks,
				UserBlocks:       prependSticky(userBlocks, stickyContext),
				System:           flattenContentBlocks(systemBlocks),
				User:             flattenContentBlocks(userBlocks),
				Model:            pCfg.Model,
				MaxTokens:        s.spawnOutputBudget(req),
				Temperature:      temp,
				FrequencyPenalty: freqPen,
				MCPServers:       mcpServers,
				Attachments:      attachments,
				Secrets:          req.Hub.Registry.Secrets,
				ForceToolCall:    forceTool,
				Constraints:      constraints,
			}, func(chunk string) error {
				streamBuffer.WriteString(chunk)
				patternWindow = append(patternWindow, chunk...)
				if len(patternWindow) > 150 {
					patternWindow = patternWindow[len(patternWindow)-150:]
				}
				if len(patternWindow) >= 150 {
					// M-9 (2026-10-04): Convert patternWindow to string once
					// instead of twice (was: string() for tail + string() for Count).
					windowStr := string(patternWindow)
					tail := windowStr[len(windowStr)-30:]
					if strings.Count(windowStr, tail) >= 4 {
						return fmt.Errorf("ErrPatternLoopBrake")
					}
				}
				return nil
			})
			cancel()

			// Record provider telemetry (ADDED 2026-10-21).
			// See docs/completed/2026-10-21/PROVIDER_ROUTING_TELEMETRY_DESIGN.md.
			if s.providerRouter != nil {
				s.providerRouter.RecordResult(pCfg.Model, time.Since(providerStart), err)
			}

			if err != nil && strings.Contains(err.Error(), "DELEGATION_REQUIRED") {
				// Defect 1 fix: include tool definitions in delegation return
				toolDefs := s.collectDelegationToolDefs(req.Hub, bindings)
				return &SpawnResult{
					Output: schemas.SubagentOutput{
						Status:     schemas.StatusDelegationRequired,
						Confidence: 1.0,
						Result: map[string]any{
							"system_prompt":    flattenContentBlocks(systemBlocks),
							"user_prompt":      flattenContentBlocks(userBlocks),
							"tool_definitions": toolDefs,
						},
						Capability: role.BaseCapability,
					},
				}, nil
			}

			// #20 (eval §8.46): Reactive context compression. When the provider
			// returns a context-limit error (prompt too long for the model's
			// window), compress the conversation history by dropping older
			// middle blocks and retry the same provider once. This prevents
			// a hard 400 crash that kills the step — the model gets a second
			// chance with a shorter context.
			if err != nil && isContextLimitError(err) {
				compressed := compressUserBlocks(userBlocks)
				if compressed != nil {
					observability.GetGlobalMetrics().Inc("orchestrator.context.compressed", 1)
					s.logger.Log("EventContextCompressed", req.TaskID, req.StepID, map[string]any{
						"turn":          turn,
						"provider":      pCfg.Provider,
						"model":         pCfg.Model,
						"blocks_before": len(userBlocks),
						"blocks_after":  len(compressed),
						"error":         err.Error(),
					})
					userBlocks = compressed

					// Retry with compressed context.
					retryCtx, retryCancel := context.WithTimeout(ctx, 120*time.Second)
					resp, err = provider.StreamComplete(retryCtx, providers.CompleteRequest{
						SystemBlocks:     systemBlocks,
						UserBlocks:       prependSticky(userBlocks, stickyContext),
						System:           flattenContentBlocks(systemBlocks),
						User:             flattenContentBlocks(userBlocks),
						Model:            pCfg.Model,
						MaxTokens:        s.spawnOutputBudget(req),
						Temperature:      temp,
						FrequencyPenalty: freqPen,
						MCPServers:       mcpServers,
						Attachments:      attachments,
						Secrets:          req.Hub.Registry.Secrets,
						ForceToolCall:    forceTool,
						Constraints:      constraints,
					}, func(chunk string) error {
						streamBuffer.WriteString(chunk)
						return nil
					})
					retryCancel()

					if err == nil {
						observability.GetGlobalMetrics().Inc("orchestrator.context.compress_retry_success", 1)
						s.logger.Log("EventContextCompressRetrySuccess", req.TaskID, req.StepID, map[string]any{
							"turn":     turn,
							"provider": pCfg.Provider,
							"model":    pCfg.Model,
						})
						effectiveProviderID = pCfg.PoolID
						effectiveModelID = pCfg.Model
						break // Turn succeeded with compressed context
					}
					// Retry also failed — fall through to normal error handling.
					s.logger.Log("EventContextCompressRetryFailed", req.TaskID, req.StepID, map[string]any{
						"turn":     turn,
						"provider": pCfg.Provider,
						"model":    pCfg.Model,
						"error":    err.Error(),
					})
				}
			}

			if err == nil {
				// Empty Response Circuit Breaker (ADDED 2026-09-18):
				// Provider returned HTTP 200 but model produced zero tokens and
				// no tool calls. This happens when vLLM lacks --tool-call-parser,
				// context overflows KV cache, or model safety filters fire.
				// Without this check, Spawner loops up to maxTurns (50) doing
				// nothing, burning wall-clock time until orchestrator_wait_task
				// timeout (90s) kills the step as "partial".
				if resp.CompletionTokens == 0 && len(resp.ToolCalls) == 0 && strings.TrimSpace(resp.Text) == "" {
					s.logger.Log(EventProviderFallback, req.TaskID, req.StepID, map[string]any{
						"turn":            turn,
						"failed_provider": pCfg.Provider,
						"failed_model":    pCfg.Model,
						"error":           "empty_response_zero_tokens",
						"prompt_tokens":   resp.PromptTokens,
						"next_candidate": func() string {
							if candIdx < len(candidateConfigs)-1 {
								return candidateConfigs[candIdx+1].providerID
							}
							return ""
						}(),
					})
					if candIdx < len(candidateConfigs)-1 {
						continue // try next candidate provider/model
					}
					// No more candidates: retry before giving up (handles intermittent
					// server-side empty responses, e.g. Xunfei MaaS §10.4).
					retry, exhaustErr := shouldRetryEmptyResponse(&emptyRespRetries, maxEmptyRespRetries)
					if retry {
						s.logger.Log(EventProviderFallback, req.TaskID, req.StepID, map[string]any{
							"error": "empty_response_zero_tokens",
							"retry": emptyRespRetries,
							"max":   maxEmptyRespRetries,
						})
						continue turnLoop
					}
					// Retries exhausted: fast-fail
					s.logger.Log(EventStepFailed, req.TaskID, req.StepID, map[string]any{
						"error":   "empty_response_zero_tokens",
						"details": fmt.Sprintf("all candidates returned 0 completion tokens after %d retries; possible causes: vLLM missing --tool-call-parser, KV cache overflow, safety filter, provider intermittent empty response", maxEmptyRespRetries),
					})
					// Persist partial trace for observability (eval §8.48.6).
					s.persistTraceOnError(ctx, req, trace, effectiveProviderID, effectiveModelID, role.BaseCapability)
					return nil, exhaustErr
				}
				effectiveProviderID = pCfg.PoolID
				effectiveModelID = pCfg.Model
				break // Turn succeeded
			}

			// Handle Error & Fallback
			if isRetryableError(err) {
				if candIdx < len(candidateConfigs)-1 {
					s.logger.Log(EventProviderFallback, req.TaskID, req.StepID, map[string]any{
						"turn":            turn,
						"failed_provider": pCfg.Provider,
						"failed_model":    pCfg.Model,
						"error":           err.Error(),
						"next_candidate":  candidateConfigs[candIdx+1].providerID,
					})

					// Trigger-driven Skill Injection (ADDED 2026-08-16)
					// If we have a skill specifically for this error, suggest it to the model.
					if s.expStore != nil {
						if repairs := s.expStore.QuerySkillsBySignal(ctx, err.Error()); len(repairs) > 0 {
							advice := fmt.Sprintf("%s: Encountered %q. Consider using these previously successful repair skills: %v", MarkerSystemAdvice, err.Error(), repairs)
							userBlocks = append(userBlocks, schemas.ContentBlock{Text: advice, CacheControl: "ephemeral"})
						}
					}
					continue
				}
			}

			// Final failure
			s.logger.Log(EventStepFailed, req.TaskID, req.StepID, map[string]any{
				"error":   "Provider call failed (all candidates exhausted)",
				"details": err.Error(),
			})
			// Persist partial trace for observability (eval §8.48.6).
			s.persistTraceOnError(ctx, req, trace, effectiveProviderID, effectiveModelID, role.BaseCapability)
			return nil, err
		}

		// Record Usage Stats
		GetGlobalStats().RecordUsage(currentPCfg.Provider, currentPCfg.Model, resp.PromptTokens, resp.CompletionTokens, len(resp.ToolCalls), resp.CacheWriteTokens, resp.CacheReadTokens)

		// Truncation observability (ADDED 2026-10-20): StopReason had no reader
		// anywhere in the tree before this, so a response cut off by the output
		// cap was indistinguishable from a model that simply stopped early.
		// That cost us a full diagnosis cycle on AppWorld 4fab96f_3, where a
		// plan ended mid-tool-name ("venmo__show_sent_payment_", 1355 chars,
		// ~400 tokens against an 8192 cap) and the only evidence available was
		// the shape of the fragment. "length" means the cap was hit and the
		// budget is the culprit; anything else means the model chose to stop and
		// raising the cap will not help.
		if resp.StopReason == "length" || resp.StopReason == "max_tokens" {
			detail := map[string]any{
				"provider":          currentPCfg.Provider,
				"model":             currentPCfg.Model,
				"stop_reason":       resp.StopReason,
				"completion_tokens": resp.CompletionTokens,
				"prompt_tokens":     resp.PromptTokens,
				"text_chars":        len(resp.Text),
				"tool_calls":        len(resp.ToolCalls),
				"max_tokens":        s.spawnOutputBudget(req),
			}
			// Distinguish the two very different ways this cap gets hit.
			//
			// A long answer spends its budget on text or tool arguments: raising
			// the cap buys more output structure. Reasoning spends it on tokens
			// that never reach Text at all, so a cap tuned for prose leaves the
			// answer with nothing — and the symptom is not "truncated answer" but
			// "model produced nothing and re-called the same tool". Flagging the
			// empty-visible case makes that diagnosable from the event alone.
			if len(resp.Text) == 0 && len(resp.ToolCalls) == 0 && resp.CompletionTokens > 0 {
				detail["visible_output_empty"] = true
				detail["likely_reasoning_dominated"] = true
			}
			s.logger.Log(EventOutputTruncated, req.TaskID, req.StepID, detail)
		}

		// Outer-ring accounting (ADDED 2026-10-12): the system-prompt profile is
		// captured at Build() time and only covers the system ring. Fold in the
		// MCP tool-schema cost and the conversation history actually sent this
		// turn, plus the provider-reported prompt/completion tokens, so
		// /api/context/profile renders the full context budget.
		historyTokens := 0
		for i := range userBlocks {
			historyTokens += providers.EstimateTokens(userBlocks[i].Text)
		}
		s.UpdateSystemPromptOuterRing(estimateMCPServersTokens(lastMCPServers), historyTokens, resp.PromptTokens, resp.CompletionTokens)

		// ── Cost Governance: Cache efficiency sentinel (P0) ───────────────
		// Design ref: docs/architecture/COST_GOVERNANCE_ACTIVE_ALERT_AND_CONTROL.md §2.2 alerter 1.
		// Pure observer: never mutates execution flow. Alerting ≠ blocking.
		if alert, n := s.cacheSentinel.Observe(req.TaskID, req.StepID, resp.PromptTokens, resp.CacheReadTokens); alert {
			s.logger.Log(EventCacheInefficiency, req.TaskID, req.StepID, map[string]any{
				"consecutive_zero_rounds": n,
				"prompt_tokens":           resp.PromptTokens,
				"cache_read_tokens":       resp.CacheReadTokens,
				"model":                   currentPCfg.Model,
				"possible_causes": []string{
					"gateway/SDK not forwarding usage.cache_read_input_tokens",
					"prompt prefix changes every turn (timestamp/random injection) breaking cache stability",
					"current model channel does not support prompt caching",
					"Anthropic cache_control markers misplaced",
				},
			})
		}

		// ── Priority 5: Capture Structured Decision Nodes ────────────────
		var turnDecisionID string
		if matches := thoughtRe.FindAllStringSubmatch(resp.Text, -1); len(matches) > 0 {
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

			if req.Hub != nil && req.Hub.Graph != nil {
				turnDecisionID = fmt.Sprintf("dec_%s_%d", req.StepID, turn)
				node := &schemas.DecisionNode{
					ID:        turnDecisionID,
					StepID:    req.StepID,
					Turn:      turn,
					Reasoning: strings.TrimSpace(reasoning.String()),
					Timestamp: time.Now(),
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

				// Basic Inference Causality: Link to context refs used in this step
				for k := range req.ContextRefs {
					node.Causes = append(node.Causes, k)
				}

				req.Hub.Graph.RecordDecision(turnDecisionID, node)
				decisionIDs = append(decisionIDs, turnDecisionID)

				// Priority 6: Learn from the new decision
				if s.expStore != nil {
					s.expStore.UpsertDecisionPrecedent(ctx, node)
				}
			}
		}

		if len(resp.ToolCalls) == 0 {
			cleanText := thoughtRe.ReplaceAllString(resp.Text, "")

			output := schemas.ParseOutput(cleanText, role.BaseCapability)
			output.Usage = &schemas.Usage{
				PromptTokens:     resp.PromptTokens,
				CompletionTokens: resp.CompletionTokens,
				TotalTokens:      resp.PromptTokens + resp.CompletionTokens,
			}
			noToolCallsYet := len(trace) == 0
			selfReportedLowConfidence := output.Status == schemas.StatusPartial && output.Confidence < 0.4

			// P1 fix (2026-10-03, eval §0.5): Detect parse failures and track consecutive count.
			// ParseOutput fallback sets Assumptions to "output not parseable" or
			// "output parseable but semantically hollow". Use this to distinguish
			// "model didn't call tools" from "model called tools but output was garbage".
			parseFailed := false
			for _, a := range output.Assumptions {
				if strings.Contains(a, "not parseable") || strings.Contains(a, "semantically hollow") {
					parseFailed = true
					break
				}
			}
			if parseFailed {
				consecutiveParseFailures++
			} else {
				consecutiveParseFailures = 0
			}

			// [EVAL-OBS] 2026-10-07 observation print: diagnose 'how many rounds the engine ran
			// within a single τ² turn, what each round called, and why it didn't exit'. Enabled by VORTEX_EVAL_OBS env var, off by default.
			if s.registry.System.FeatureFlags.EvalObs {
				fmt.Fprintf(os.Stderr, "[EVAL-OBS] task=%s step=%s turn=%d/%d calls=%d trace=%d status=%s conf=%.2f parse=%v(consec=%d) noTool=%v lowConf=%v\n",
					req.TaskID, req.StepID, turn+1, maxTurns, len(resp.ToolCalls), len(trace),
					output.Status, output.Confidence, parseFailed, consecutiveParseFailures, noToolCallsYet, selfReportedLowConfidence)
				for _, tc := range resp.ToolCalls {
					fmt.Fprintf(os.Stderr, "[EVAL-OBS]   -> %s %v\n", tc.Name, tc.Arguments)
				}
				if strings.TrimSpace(cleanText) != "" {
					txt := strings.TrimSpace(cleanText)
					if len(txt) > 200 {
						txt = txt[:200]
					}
					fmt.Fprintf(os.Stderr, "[EVAL-OBS]   text: %s\n", strings.ReplaceAll(txt, "\n", " | "))
				}
			}

			// P1 fix (2026-10-03, eval §0.5): Spin breaker.
			// After N consecutive parse failures, stop spinning and return the
			// best partial result. Burning the entire turn budget on identical
			// failures wastes 19-43 provider calls for zero benefit.
			if consecutiveParseFailures >= maxConsecutiveParseFailures {
				output.Warnings = append(output.Warnings, fmt.Sprintf(
					"orchestrator: broke spin loop after %d consecutive parse failures", consecutiveParseFailures))
				s.logger.Log(EventProviderFallback, req.TaskID, req.StepID, map[string]any{
					"turn":                       turn,
					"consecutive_parse_failures": consecutiveParseFailures,
					"action":                     "spin_break",
				})
				observability.GetGlobalMetrics().Inc("orchestrator.spin.broken", 1)
				return &SpawnResult{
					Output:        output,
					ProviderID:    effectiveProviderID,
					ModelID:       effectiveModelID,
					Ref:           req.TaskID + ":" + req.StepID,
					TurnsUsed:     turn + 1,
					StatesVisited: statesVisited,
				}, nil
			}

			// FIX (2026-09-07): Malformatted Tool Call Guard.
			// When the model outputs pseudo-XML (<anim:call>, <tool_code>) instead of real JSON tool calls,
			// intercept it and inject a stern correction prompt so it doesn't get trapped.
			if hasMalformedToolCallTag(cleanText) {
				msg := SpawnerInvalidToolFormatMsg
				for i := range userBlocks {
					userBlocks[i].CacheControl = ""
				}
				userBlocks = append(userBlocks, schemas.ContentBlock{Text: cleanText})
				userBlocks = append(userBlocks, schemas.ContentBlock{Text: msg, CacheControl: "ephemeral"})
				continue
			}
			// WITHOUT ever calling a tool. Once the model has made tool calls
			// (len(trace) > 0), subsequent text is legitimate chain-of-thought
			// reasoning — not an early exit. Let the model declare completion
			// on its own; Auditor (verifyExitCriteria) will catch incomplete
			// results at the exit gate and trigger auto-refine. This preserves
			// the model's ability to reason between tool calls, which is
			// essential for multi-step DAGs.
			// A7 (eval FINDINGS.md §A7, 2026-10-22): DisableForceTool is NOT
			// consulted here. It disables the provider-side output constraint
			// (turn-0 "must call a tool" grammar, :323); this gate is the
			// prompt-side reminder for a model that is spinning without doing
			// anything. "Give the model freedom from the forced-tool constraint"
			// and "remind a motionless model to act" are different mechanisms —
			// turning off the former must not silence the latter. (Not triggered
			// in the v20 configs — the flag was absent — but the coupling would
			// bite the first deployment that sets it.)
			if turn < maxTurns-1 && noToolCallsYet && (selfReportedLowConfidence || turn == 0) {
				if s.registry.System.FeatureFlags.EvalObs {
					fmt.Fprintf(os.Stderr, "[EVAL-OBS] NUDGE triggered turn=%d noTool=%v lowConf=%v parse=%v\n",
						turn+1, noToolCallsYet, selfReportedLowConfidence, parseFailed)
				}
				msg := SpawnerNoToolsMsg
				if output.Status == schemas.StatusCapabilityRequired && len(output.RequiredCapabilities) > 0 {
					msg = fmt.Sprintf("%s Before finalizing capability_required, double-check your bound tools. If none fulfill %v, explain and finalize.", MarkerSystem, output.RequiredCapabilities)
				}
				// P1 fix (2026-10-03, eval §0.5): When the spin is caused by parse
				// failure (not just "no tools"), include format guidance so the model
				// knows BOTH problems: (1) call tools, (2) output valid JSON.
				// Without this, the model retries with the same bad format → spin.
				if parseFailed {
					msg = SpawnerNoToolsBadJSONMsg
				}
				for i := range userBlocks {
					userBlocks[i].CacheControl = ""
				}
				userBlocks = append(userBlocks, schemas.ContentBlock{Text: cleanText})
				userBlocks = append(userBlocks, schemas.ContentBlock{Text: msg, CacheControl: "ephemeral"})
				continue
			}

			// ── Zero-effect completion gate (ADDED 2026-10-21, eval v14) ──────
			// `noToolCallsYet` only asks "did the model call anything at all".
			// A single read-only call (show_profile, get_balance, search_*) is
			// enough to defeat it, after which the model may declare status=ok at
			// 0.90 confidence and the step is accepted with no state change.
			//
			// AppWorld evidence: task d4e9306_1 ran five times on one config; in
			// 4/5 the trace held exactly [supervisor__show_profile,
			// supervisor__show_account_passwords] and the model still wrote "Now
			// following all artists..." — scored 1/6 each time.
			//
			// Deliberately one-shot and opt-in. Read-only tasks legitimately hit
			// this condition, so the injected text asks the model to *classify*
			// its own step rather than asserting it did nothing. Re-firing would
			// turn a genuine "this was a query" answer into a loop.
			//
			// A2 (eval FINDINGS.md §A2, 2026-10-22): the gate also fires when
			// parseFailed — ParseOutput degraded the output to partial (conf
			// 0.3), which is the engine's own artifact, not the model declaring
			// completion. Requiring StatusOK left the v20 format collapse with
			// zero corrective signal (ZERO-EFFECT NUDGE fired 0 times in 8
			// tasks): A1's guard catches the KNOWN pseudo-tags before parsing,
			// this arm catches whatever malformed shapes the future invents
			// after ParseOutput degrades them. parse-failure turns get their own
			// message variant — the ok-variant would assert "you declared
			// status=ok" about text the engine could not read.
			if s.registry.System.FeatureFlags.ZeroEffectNudge && !zeroEffectNudged && !noToolCallsYet &&
				(output.Status == schemas.StatusOK || parseFailed) && turn < maxTurns-1 {
				hasEffect := false
				seenTools := make([]string, 0, len(trace))
				for _, t := range trace {
					seenTools = append(seenTools, t.ToolName)
					if !IsReadOnlyToolName(t.ToolName) {
						hasEffect = true
					}
				}
				if !hasEffect {
					zeroEffectNudged = true
					s.logger.Log("EventZeroEffectNudged", req.TaskID, req.StepID, map[string]any{
						"turn":         turn + 1,
						"trace_len":    len(trace),
						"trace_tools":  seenTools,
						"confidence":   output.Confidence,
						"parse_failed": parseFailed,
					})
					observability.GetGlobalMetrics().Inc("orchestrator.zero_effect.nudged", 1)
					if s.registry.System.FeatureFlags.EvalObs {
						fmt.Fprintf(os.Stderr, "[EVAL-OBS] ZERO-EFFECT NUDGE turn=%d trace=%d conf=%.2f parse=%v tools=%v\n",
							turn+1, len(trace), output.Confidence, parseFailed, seenTools)
					}
					for i := range userBlocks {
						userBlocks[i].CacheControl = ""
					}
					userBlocks = append(userBlocks, schemas.ContentBlock{Text: cleanText})
					msg := SpawnerZeroEffectMsg
					if parseFailed {
						msg = SpawnerZeroEffectParseFailMsg
					}
					userBlocks = append(userBlocks, schemas.ContentBlock{Text: msg, CacheControl: "ephemeral"})
					continue
				}
			}

			if noToolCallsYet && output.Status == schemas.StatusOK {
				output.Status = schemas.StatusPartial
				if output.Confidence > 0.4 {
					output.Confidence = 0.4
				}
				output.Warnings = append(output.Warnings, "orchestrator: model reported status=ok but no tool calls were recorded; downgraded to partial")
			}

			// ── Mid-abandonment gate (A5, eval FINDINGS.md §A5, 2026-10-22) ──
			// The highest-frequency small-model failure mode: the model starts
			// working (read-only calls land in the trace), hits friction (a
			// 422, an empty page), then emits a PARSEABLE partial and stops
			// while turns remain. All three earlier gates are blind to it:
			// noToolCallsYet is false (work started), the zero-effect gate
			// wants ok/parseFailed (this is a clean partial), and without this
			// gate the partial is accepted as-is right below.
			//
			// Deliberately one-shot and opt-in (mid_abandon_nudge), like the
			// zero-effect gate. `!parseFailed` keeps the message honest — a
			// parse-collapsed turn gets the zero-effect parse-fail variant
			// instead (A2), not "you reported this step as incomplete", which
			// would assert the engine read an output it did not. StatusFailed
			// is excluded on purpose: parseable failures already route through
			// the in-loop failure/retry machinery. The message leaves a
			// legitimate exit open — a genuinely blocked model is told to
			// finalize with missing_context, not forced into a loop.
			if s.registry.System.FeatureFlags.MidAbandonNudge && !midAbandonNudged && !noToolCallsYet &&
				output.Status == schemas.StatusPartial && !parseFailed && turn < maxTurns-1 &&
				!traceHasEffect(trace) {
				midAbandonNudged = true
				seenTools := make([]string, 0, len(trace))
				for _, t := range trace {
					seenTools = append(seenTools, t.ToolName)
				}
				s.logger.Log("EventMidAbandonNudged", req.TaskID, req.StepID, map[string]any{
					"turn":        turn + 1,
					"trace_len":   len(trace),
					"trace_tools": seenTools,
					"confidence":  output.Confidence,
				})
				observability.GetGlobalMetrics().Inc("orchestrator.mid_abandon.nudged", 1)
				if s.registry.System.FeatureFlags.EvalObs {
					fmt.Fprintf(os.Stderr, "[EVAL-OBS] MID-ABANDON NUDGE turn=%d trace=%d conf=%.2f tools=%v\n",
						turn+1, len(trace), output.Confidence, seenTools)
				}
				for i := range userBlocks {
					userBlocks[i].CacheControl = ""
				}
				userBlocks = append(userBlocks, schemas.ContentBlock{Text: cleanText})
				userBlocks = append(userBlocks, schemas.ContentBlock{Text: SpawnerMidAbandonMsg, CacheControl: "ephemeral"})
				continue
			}

			// ── Priority 5: Update Decision Outcome (Final) ──────────────────
			if turnDecisionID != "" && req.Hub != nil && req.Hub.Graph != nil {
				req.Hub.Graph.UpdateDecisionOutcome(turnDecisionID, string(output.Status))
			}

			s.taskStoreSet(ctx, req.TaskID, req.StepID, &store.StepResult{
				Data:           output.Result,
				Confidence:     output.Confidence,
				MissingContext: output.MissingContext,
				Capability:     role.BaseCapability,
				ProviderID:     effectiveProviderID,
				ModelID:        effectiveModelID,
				Attachments:    output.Attachments,
				Trace:          trace,
				DecisionIDs:    decisionIDs,
				StatesVisited:  statesVisited,
				CreatedAt:      time.Now(),
			})

			return &SpawnResult{
				Output:        output,
				ProviderID:    effectiveProviderID,
				ModelID:       effectiveModelID,
				Ref:           req.TaskID + ":" + req.StepID,
				StatesVisited: statesVisited,
				TurnsUsed:     turn + 1,
			}, nil
		}

		// Handle Tool Calls
		var toolResults []string
		var turnAttachments []schemas.Attachment
		needsVerification := false

		// ── Sieve Guardian: Tool Loop Detection (Two-Tier Escalation) ───────
		if valid, reason := s.checkSieveGuardian(req.TaskID, req.StepID, turn, resp.ToolCalls, trace); !valid {
			sieveGuardianHits++
			observability.GetGlobalMetrics().Inc("orchestrator.sieve.interceptions", 1)
			if sieveGuardianHits == 1 {
				userBlocks = append(userBlocks, schemas.ContentBlock{
					Text: fmt.Sprintf(SpawnerLoopDetectedFmt, reason),
				})
				continue
			}
			if noToolProvider, perr := providers.Get(currentPCfg, req.Hub.Registry.ExternalRuntimes); perr == nil {
				noToolResp, nerr := noToolProvider.StreamComplete(ctx, providers.CompleteRequest{
					UserBlocks: append(userBlocks, schemas.ContentBlock{
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
					return &SpawnResult{
						Output:        degradedOutput,
						ProviderID:    effectiveProviderID,
						ModelID:       effectiveModelID,
						Ref:           req.TaskID + ":" + req.StepID,
						TurnsUsed:     turn + 2,
						StatesVisited: statesVisited,
					}, nil
				}
			}
			return s.failedOutput(role.BaseCapability, reason), nil
		}

		// Execute tool calls via extracted method (Option B split).
		tcState := &toolExecState{
			toolResults:       toolResults,
			trace:             trace,
			toolCallCount:     toolCallCount,
			toolFailCount:     toolFailCount,
			resultStreak:      resultStreak,
			resultLastHash:    resultLastHash,
			needsVerification: needsVerification,
			turnAttachments:   turnAttachments,
			statesVisited:     statesVisited,
			previousState:     previousState,
		}
		s.processToolCalls(ctx, req, resp, bindings, turnDecisionID, turnCtx, tcState)
		toolResults = tcState.toolResults
		trace = tcState.trace
		toolCallCount = tcState.toolCallCount
		toolFailCount = tcState.toolFailCount
		resultStreak = tcState.resultStreak
		resultLastHash = tcState.resultLastHash
		needsVerification = tcState.needsVerification
		turnAttachments = tcState.turnAttachments
		statesVisited = tcState.statesVisited
		previousState = tcState.previousState

		// R2: Track tools called across turns for sticky union.
		for _, tc := range resp.ToolCalls {
			calledTools[tc.Name] = true
		}

		// B2 (eval §3.2) + C3 fix: Output hash dedup — detect model stuck
		// re-fetching identical data (e.g. 129 venmo calls inside execute_code
		// loops). Consecutive-only: if the hash changes, count resets. The hash
		// includes tool call names + args (not just results) so different calls
		// with the same output don't collide. Short/error-only turns are skipped.
		// On failure, returns partial output (not failedOutput) to preserve
		// accumulated work for downstream steps.
		if len(toolResults) > 0 {
			turnHash, shouldTrack := computeTurnHash(resp.ToolCalls, toolResults)
			if shouldTrack {
				if turnHash == lastResultHash {
					consecutiveRepeatCount++
				} else {
					lastResultHash = turnHash
					consecutiveRepeatCount = 1
				}
				if consecutiveRepeatCount > maxConsecutiveDuplicates {
					if !outputStagnationHinted {
						outputStagnationHinted = true
						userBlocks = append(userBlocks, schemas.ContentBlock{
							Text: "System: You have produced identical tool results multiple times. You appear to be stuck re-fetching the same data. Summarize what you have found so far and proceed to complete the task. Do not repeat the same tool calls.",
						})
						s.logger.Log("EventOutputStagnationHinted", req.TaskID, req.StepID, map[string]any{
							"turn": turn, "consecutive_count": consecutiveRepeatCount,
						})
					} else {
						s.logger.Log(EventStepFailed, req.TaskID, req.StepID, map[string]any{
							"error":             "output_stagnation: identical tool results repeated after hint",
							"consecutive_count": consecutiveRepeatCount,
							"root_cause":        "model_stuck",
						})
						// A8 (eval FINDINGS.md §A8, 2026-10-22): the hint stage logs
						// EventOutputStagnationHinted, but the kill itself was invisible
						// to EVAL-OBS greps — after the hint, whether a task died and how
						// it ended could not be answered from the eval logs. EventStepFailed
						// above is always emitted; this only adds the eval channel.
						if s.registry.System.FeatureFlags.EvalObs {
							fmt.Fprintf(os.Stderr, "[EVAL-OBS] STAGNATION KILL task=%s step=%s turn=%d trace=%d repeats=%d\n",
								req.TaskID, req.StepID, turn+1, len(trace), consecutiveRepeatCount)
						}
						return s.partialStagnationOutput(role.BaseCapability,
							fmt.Sprintf("output_stagnation: identical tool results repeated %d consecutive times after intervention hint", consecutiveRepeatCount),
							trace), nil
					}
				}
			} else {
				// Different turn (short/error-only) — reset consecutive tracking
				// so a legitimate short result between two identical long turns
				// doesn't bridge them into a false positive.
				lastResultHash = 0
				consecutiveRepeatCount = 0
			}
		}

		// Accumulate sticky results (auth/login) into stickyContext for
		// prepending to subsequent turns. Dedup by substring containment.
		stickyContext = accumulateSticky(stickyContext, tcState.stickyResults)

		// ── Priority 3: Gateway Watchdog (Active Conflict Probing) ────────
		if alert := s.watchdog.Sniff(resp.ToolCalls, req.Hub); alert != nil {
			toolResults = append(toolResults, fmt.Sprintf("\n%s", alert.Message))
			s.logger.Log(EventSwarmWatchdogTriggered, req.TaskID, req.StepID, map[string]any{
				"type": alert.Type,
			})
		}

		// ── VDA: Forced Visual Audit ──────────────────────────────────────
		if needsVerification {
			for _, b := range bindings {
				mcp := req.Hub.GetMCP(b.MCPID)
				if mcp == nil {
					continue
				}
				if containsTool(mcp.AvailableTools, "capture_screenshot") {
					cli, err := s.mcpMgr.ensureMCPClient(ctx, mcp, req.TaskID)
					if err == nil {
						screen, err := cli.SendRequest(ctx, "tools/call", map[string]any{
							"name":      "capture_screenshot",
							"arguments": map[string]any{},
						})
						if err == nil {
							if att, ok := screen.(schemas.SubagentOutput); ok {
								turnAttachments = append(turnAttachments, att.Attachments...)
							}
							toolResults = append(toolResults, "System: Automatic verification screenshot captured.")
						}
					}
					break
				}
			}
		}

		s.persistAttachments(req.TaskID, req.StepID, turnAttachments)

		// ── Priority 5: Update Decision Outcome ──────────────────────────
		if turnDecisionID != "" && req.Hub != nil && req.Hub.Graph != nil {
			req.Hub.Graph.UpdateDecisionOutcome(turnDecisionID, fmt.Sprintf("executed %d tools", len(resp.ToolCalls)))
		}

		// Priority 4: Re-route tools for the next turn based on updated context
		// (Progressive Disclosure).
		// P1 fix (2026-10-20): Only re-route on the first turn (turn == 0).
		// Re-routing every turn changes the tool block → prompt cache miss
		// every turn. For turns > 0, keep existing bindings and grow via
		// sticky union only (append-only surface). Progressive discovery
		// on later turns is handled by the decision gate's refine_and_retry.
		if turn == 0 {
			bindings = s.toolRouter.Route(ctx, RouteRequest{
				Task:               req.Task + " " + resp.Text,
				Bindings:           initialBindings,
				Turn:               turn + 1,
				ProgressiveMode:    progressiveEnabled,
				PredecessorSkillID: req.PredecessorSkillID,
				MaxTokenBudget:     s.toolSchemaBudget(),
				TaskID:             req.TaskID,
			})
		}

		// R2: Sticky union — re-route may drop tools the model was actively
		// using. Add them back so multi-turn workflows don't break.
		bindings = s.stickyUnionBindings(bindings, initialBindings, calledTools)

		vdaPrompt := ""
		if needsVerification {
			vdaPrompt = SpawnerVDAPrompt
		}

		// Update history blocks - Rolling Marker strategy
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

		// #15 (eval §8.35): No-progress turn detection. If this turn had tool
		// calls but ALL were fs-only (no domain MCP calls), increment the
		// streak. Otherwise reset. Emit metric so an arm can validate before
		// adding a threshold/action.
		if len(resp.ToolCalls) > 0 {
			allFsOnly := true
			for _, tc := range resp.ToolCalls {
				if !CoreToolNames[tc.Name] {
					allFsOnly = false
					break
				}
			}
			if allFsOnly {
				fsOnlyTurnStreak++
			} else {
				if fsOnlyTurnStreak > 0 {
					observability.GetGlobalMetrics().Inc("orchestrator.progress.fs_only_streak_max", int64(fsOnlyTurnStreak))
				}
				fsOnlyTurnStreak = 0
			}
			if fsOnlyTurnStreak >= 2 {
				observability.GetGlobalMetrics().Inc("orchestrator.progress.stalled_turns", 1)
			}
			// #4+#15 (eval §8.37.2/§8.40): Break the turn loop if the fs-only
			// streak exceeds the configured limit. The model is stuck in a
			// write→read self-proof loop with no domain progress.
			if fsOnlyStreakLimit > 0 && fsOnlyTurnStreak >= fsOnlyStreakLimit {
				observability.GetGlobalMetrics().Inc("orchestrator.progress.fs_only_stall_break", 1)
				s.logger.Log("EventFsOnlyStallBreak", req.TaskID, req.StepID, map[string]any{
					"turn":         turn,
					"streak":       fsOnlyTurnStreak,
					"limit":        fsOnlyStreakLimit,
					"max_turns":    maxTurns,
					"tool_results": len(toolResults),
				})
				// Persist partial trace for observability (eval §8.48.6).
				s.persistTraceOnError(ctx, req, trace, effectiveProviderID, effectiveModelID, role.BaseCapability)
				return nil, fmt.Errorf("fs_only_stall: %d consecutive turns used only filesystem tools (write_file/read_file/execute_code) without domain MCP calls; breaking to conserve budget (limit=%d)", fsOnlyTurnStreak, fsOnlyStreakLimit)
			}
		}
	}

	// FIX (2026-09-07): instead of hard-failing, emit a sentinel error the
	// scheduler converts into a resumable pending decision
	// (schemas.DecisionMaxTurnsExhausted). The step keeps its partial progress
	// (trace is persisted as the step's tool interactions) so a resume can
	// continue from here rather than restarting blind.
	// FIX (eval §8.48.6): the comment above claimed trace was persisted, but
	// no taskStoreSet call existed. Now actually persist it.
	s.persistTraceOnError(ctx, req, trace, effectiveProviderID, effectiveModelID, role.BaseCapability)
	return nil, fmt.Errorf("%s: tool execution loop reached max turns (%d) without a final answer; %d tool interaction(s) executed",
		ErrMaxTurnsExhausted, maxTurns, len(trace))
}
