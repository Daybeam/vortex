package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/client"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/registry"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ── Package-level compiled regex (PB-3: avoid recompiling per turn) ────────
var thoughtRe = regexp.MustCompile("(?s)<thought>(.*?)</thought>")

// ── Error Sentinels ────────────────────────────────────────────────────────
//
// Sentinel errors the scheduler inspects via strings.Contains, mirroring the
// existing "DYNAMIC_ESCALATION_REQUIRED" convention in scheduler_dag.go.
const (
	// ErrMaxTurnsExhausted is returned when a step's tool-execution loop hits
	// maxTurns without a final answer. The scheduler converts this into a
	// resumable pending decision (see scheduler_dag.go) rather than hard-failing
	// the step, so an operator or Main Agent can resume with more turns.
	ErrMaxTurnsExhausted = "MAX_TURNS_EXHAUSTED"
)

// Spawner builds subagent prompts, calls the provider, and parses output.
type Spawner struct {
	registry          *config.Registry
	taskStore         store.ITaskStore
	expStore          store.IExperienceStore
	logger            *Logger
	mcpMgr            *MCPConnectionManager
	Mu                sync.Mutex
	interceptors      []Interceptor
	contextProviders  []ContextProvider
	outputBase        string
	ResourceLoader    *ResourceLoader
	processor         *OutputProcessor
	toolRouter        *ToolRouter
	skillRouter       *SkillRouter
	verifierRegistry  map[string]Verifier
	constraintAdapter *ConstraintAdapter
	sieve             *Sieve
	contextManager    *ContextManager
	CaSKG             *CaSKGManager
	watchdog          *GatewayWatchdog
	healer            *Healer
	pa                *PromptAssembler

	assets                   *AssetManager
	refBasedHandoffThreshold int
	SignalField              *SignalField
	irtEstimator             *IRTBudgetEstimator
	cacheSentinel            *CacheSentinel
	allowedWorkspaces        []string
	capProfileStore          *store.CapabilityProfileStore
	capLookup                providers.CapabilityProfileLookup
	modelRegistry            *registry.ModelRegistry
}

func NewSpawner(reg *config.Registry, ts store.ITaskStore, es store.IExperienceStore, logger *Logger, loader *ResourceLoader, outputBase string) *Spawner {
	interceptors := []Interceptor{
		AuditInterceptor(logger), // FIRST: wrap everything in audit
		HealthCheckInterceptor(reg, logger),
		LogInterceptor(reg, logger, NewRoleGenerator(reg, loader, logger)),
		NewAntiSlopInterceptor(reg, logger).Wrap,
	}

	// Multi-language Governance Interceptors (ADDED 2026-07-14)
	for _, scriptPath := range reg.System.ExternalInterceptors {
		interceptors = append(interceptors, NewExternalInterceptor(scriptPath, reg.ExternalRuntimes, logger))
	}

	caskg := NewCaSKGManager(filepath.Join("data", "causal_skill_graph.json"))
	caskg.HookEventBus(DefaultBus)

	spawner := &Spawner{
		registry:         reg,
		taskStore:        ts,
		expStore:         es,
		logger:           logger,
		ResourceLoader:   loader,
		mcpMgr:           NewMCPConnectionManager(reg, logger),
		verifierRegistry: make(map[string]Verifier),
		interceptors:     interceptors,
		contextProviders: []ContextProvider{
			&TimeProvider{},
		},
		outputBase:        outputBase,
		processor:         NewOutputProcessor(),
		toolRouter:        NewToolRouter(reg, caskg),
		skillRouter:       NewSkillRouter(reg),
		constraintAdapter: NewConstraintAdapter(),
		CaSKG:             caskg,
		sieve:             newSieveFromConfig(reg.System),
		contextManager:    NewContextManager(reg),
		watchdog:          NewGatewayWatchdog(reg),
		healer:            NewHealer(),
		pa:                NewPromptAssembler(es, loader, reg, logger),
		irtEstimator:      NewIRTBudgetEstimator(50),
		cacheSentinel:     NewCacheSentinel(),
		allowedWorkspaces: reg.System.Sandbox.AllowedWorkspaces,
	}

	// Priority 6: Wire up embedding client for Experience Store
	if es != nil {
		hub := NewContextHub(reg, nil, es)
		if esStore, ok := es.(*store.ExperienceStore); ok {
			esStore.SetEmbeddingClient(hub.GetEmbeddingClient())
		}
	}

	spawner.CaSKG.HookEventBus(DefaultBus)
	return spawner
}

// AddInterceptor adds an interceptor to the end of the chain.

// AddInterceptor adds an interceptor to the end of the chain.
func (s *Spawner) AddInterceptor(i Interceptor) {
	s.interceptors = append(s.interceptors, i)
}

// SetCapabilityProfileStore wires the per-model per-capability telemetry store.
// When set, Spawn records each execution outcome for Pareto routing and IRT theta.
func (s *Spawner) SetCapabilityProfileStore(cps *store.CapabilityProfileStore) {
	s.capProfileStore = cps
	s.capLookup = NewCapProfileLookup(cps)
}

// SetModelRegistry wires the pluggable model registry for context-window-aware
// prompt budgeting. When set, GetEffectiveHandoffThreshold falls back to the
// registry's embedded metadata when pCfg.MaxContextWindow is unset.
func (s *Spawner) SetModelRegistry(mr *registry.ModelRegistry) {
	s.modelRegistry = mr
}

// AddContextProvider adds a provider for dynamic prompt injection.

// AddContextProvider adds a provider for dynamic prompt injection.
func (s *Spawner) AddContextProvider(cp ContextProvider) {
	s.contextProviders = append(s.contextProviders, cp)
}

// SetAssetManager wires in the DirectedEngine's AssetManager so that large
// upstream contexts can be side-loaded into artifact files. Anti-Cliff
// (ADDED 2026-08-27).

// SetAssetManager wires in the DirectedEngine's AssetManager so that large
// upstream contexts can be side-loaded into artifact files. Anti-Cliff
// (ADDED 2026-08-27).
func (s *Spawner) SetAssetManager(am *AssetManager) {
	s.assets = am
}

// SetRefBasedHandoffThreshold sets the byte threshold above which an
// upstream context is side-loaded into an artifact file rather than
// inlined into the subagent's prompt. Zero or negative means the
// mechanism is disabled (legacy inline behaviour). Anti-Cliff (ADDED
// 2026-08-27).

// SetRefBasedHandoffThreshold sets the byte threshold above which an
// upstream context is side-loaded into an artifact file rather than
// inlined into the subagent's prompt. Zero or negative means the
// mechanism is disabled (legacy inline behaviour). Anti-Cliff (ADDED
// 2026-08-27).
func (s *Spawner) SetRefBasedHandoffThreshold(bytes int) {
	s.refBasedHandoffThreshold = bytes
}

// CloseMCPConnections closes all cached MCP subprocess clients. Call on
// shutdown to prevent orphaned subprocesses (audit finding C2).
func (s *Spawner) CloseMCPConnections() {
	s.mcpMgr.CloseAll()
}

// SpawnResult is the outcome of one subagent call.

// SpawnResult is the outcome of one subagent call.
type SpawnResult struct {
	Output        schemas.SubagentOutput
	ProviderID    string   // 实际使用的 Provider ID
	ModelID       string   // 实际使用的模型 ID
	Ref           string   // "task_id:step_id"
	StatesVisited []string // PGPO: Visited environment states (ADDED 2026-09-08)
	TurnsUsed     int      // Actual tool-call turns consumed (ADDED 2026-09-13 IRT)
}

// Spawn executes one step as a subagent call.
// Does NOT handle retries — that lives in the scheduler.

// Spawn executes one step as a subagent call.
// Does NOT handle retries — that lives in the scheduler.
func (s *Spawner) Spawn(ctx context.Context, req *SpawnRequest) (*SpawnResult, error) {
	if req.TraceID == "" {
		req.TraceID = GetTraceID(ctx)
		if req.TraceID == "" {
			req.TraceID = NewTraceID()
		}
	}
	if req.SpanID == "" {
		req.SpanID = NewSpanID()
	}
	ctx = ContextWithTrace(ctx, req.TraceID, req.SpanID)

	handler := BuildInterceptorChain(s.interceptors, s.doSpawn)
	start := time.Now()
	result, err := handler(ctx, req)
	if err == nil && result != nil {
		s.recordCapabilityOutcome(ctx, req, result, time.Since(start))
	}
	return result, err
}

// doSpawn is the core spawning logic, wrapped by interceptors.

// doSpawn is the core spawning logic, wrapped by interceptors.
func (s *Spawner) doSpawn(ctx context.Context, req *SpawnRequest) (*SpawnResult, error) {
	// Ensure Hub exists (safety fallback)
	if req.Hub == nil {
		req.Hub = NewContextHub(s.registry, nil, s.expStore)
	}

	role := req.Hub.GetRole(req.RoleID)
	if role == nil {
		return nil, fmt.Errorf("role %q not found", req.RoleID)
	}

	// ─── Capability Manifest Pre-flight Validation (Phase 2) ────────────────
	if err := s.validateCapabilities(ctx, req, role); err != nil {
		return nil, err
	}

	// == Phase 0: Skill Retrieval (RAG for Skills) ==========================
	// If the role allows dynamic skills, automatically find and add relevant autogenerated ones
	additionalSkills := s.retrieveDynamicSkills(ctx, req, role)

	// Prepare candidates
	candidateConfigs := s.buildCandidateConfigs(ctx, req, role)

	// Resolve skills
	finalSkills, err := ResolveFinalSkills(req.Hub, req.RoleID, additionalSkills)
	if err != nil {
		return s.failedOutput(role.BaseCapability, err.Error()), nil
	}

	// Resolve MCP bindings
	var bindings []config.MCPBinding
	if req.RoutingMode == schemas.RoutingModeManifest {
		allCaps := append([]string{}, role.Requires...)
		allCaps = append(allCaps, role.Optional...)
		bindings = req.Hub.Registry.ResolveCapabilitiesToBindings(allCaps)

		// Strict validation for Requires: ensure all required capabilities are satisfied by the mounted tools.
		// CheckCapabilities returns missing strings if they are NOT satisfied by any tool in ANY mounted MCP.
		missing := req.Hub.Registry.CheckCapabilities(role.Requires)
		if len(missing) > 0 {
			return s.failedOutput(role.BaseCapability, fmt.Sprintf("Blocked: manifest routing failed. Missing required capabilities: %v", missing)), nil
		}
	} else {
		var err error
		bindings, err = ResolveFinalMCPs(req.Hub, req.RoleID, req.AdditionalMCPs, req.AdditionalToolAllowlists)
		if err != nil {
			return s.failedOutput(role.BaseCapability, err.Error()), nil
		}
	}
	initialBindings := bindings // Save full set for progressive re-routing

	// Priority 4: Determine if progressive disclosure should be enabled.
	progressiveEnabled := true
	if strings.Contains(strings.ToLower(req.Task), "fix") || strings.Contains(strings.ToLower(req.Task), "implement") {
		progressiveEnabled = false
	}

	if req.RoutingMode != schemas.RoutingModeManifest {
		// ─── Phase 0.5: Dynamic Tool Sieve (Skill Affinity) ──────────────────
		// Filter tools based on task description to reduce schema noise
		bindings = s.toolRouter.Route(RouteRequest{
			Task:            req.Task,
			Bindings:        bindings,
			Turn:            0,
			ProgressiveMode: progressiveEnabled,
		})
	}

	// Priority 6: Semantic Precedent Retrieval (Turn 0)
	var precedents []*schemas.DecisionNode
	if s.expStore != nil && progressiveEnabled {
		precedents = s.expStore.QueryDecisionPrecedents(ctx, req.Task, 2)
		if len(precedents) > 0 {
			s.logger.Log("EventSemanticPrecedentsFound", req.TaskID, req.StepID, map[string]any{
				"count": len(precedents),
			})
		}
	}

	// Collect flat allowed tools for prompt injection
	toolConstraints := collectToolConstraints(bindings)

	// Resolve context refs from KV
	resolvedCtx := s.resolveContextRefs(ctx, req)

	// == Phase 0.7: Tree Path Context =======================================
	var pathContext []map[string]any
	if path, ok := req.Metadata["context_tree_path"].([]map[string]any); ok {
		pathContext = path
	}

	// ─── Phase 1.5: Context Compression (Headroom Inspired) ────────────
	// Shrink large code blocks in upstream context to save tokens.
	if s.sieve != nil {
		resolvedCtx = s.sieve.CompressContext(resolvedCtx)
	}

	// == Phase 1: Dynamic Context Injection ==================================
	// Fetch real-time context from ContextProviders
	envContext := s.fetchDynamicContext(ctx, req.TaskID, req.StepID)

	// Merge with resolvedCtx (upstream results)
	mergedContext := map[string]any{
		"env":      envContext,
		"upstream": resolvedCtx,
		"path":     pathContext, // Tree-based path
		"task": map[string]any{
			"id":   req.TaskID,
			"step": req.StepID,
			"role": req.RoleID,
		},
	}

	// Build system prompt using template
	prunedSkills := s.skillRouter.Route(SkillRouteRequest{
		Task:            req.Task,
		AvailableSkills: finalSkills,
		Turn:            0,
		ProgressiveMode: progressiveEnabled,
	})

	// Gather Few-Shot examples for the routed tools
	toolFewShots := gatherToolFewShots(req.Hub, bindings)

	// ─── Phase 1: Dynamic Context Injection ───────────────────────────────
	// Anti-Cliff (ADDED 2026-08-27, arXiv:2608.22752):
	//   REF-BASED HANDOFF: if the resolved upstream context exceeds the
	//   threshold, side-load into an artifact file and inject only the
	//   pointer + summary (never the raw payload), preserving subagent
	//   lazy-load access.
	userText := req.Task
	if req.AdditionalPromptContext != "" {
		userText = fmt.Sprintf("%s\n\n[RECOVERY CONTEXT]\n%s", userText, req.AdditionalPromptContext)
	}
	userText = s.applyRefBasedHandoff(ctx, req, role, userText, resolvedCtx)
	var userBlocks []schemas.ContentBlock
	userBlocks = append(userBlocks, schemas.ContentBlock{Text: userText, CacheControl: "ephemeral"})

	s.logger.Log(EventStepStarted, req.TaskID, req.StepID, map[string]any{
		"role":   req.RoleID,
		"skills": finalSkills,
		"mcp_bindings": func() []map[string]any {
			out := make([]map[string]any, len(bindings))
			for i, b := range bindings {
				tools := any("*")
				if b.IsRestricted() {
					tools = b.AllowedTools
				}
				mcpDef := req.Hub.GetMCP(b.MCPID)
				tc := -1
				if mcpDef != nil {
					tc = len(mcpDef.FullToolDefinitions)
				}
				out[i] = map[string]any{"mcp": b.MCPID, "allowed_tools": tools, "tool_defs": tc}
			}
			return out
		}(),
	})

	// ─── Subagent Delegation Mode (Zero-Config Cold Start) - ADDED 2026-08-19 ───
	if result, handled := s.tryDelegationMode(ctx, req, role, prunedSkills, toolConstraints, mergedContext, toolFewShots, userText, bindings); handled {
		return result, nil
	}

	// == Tool Execution Loop (VDA Enhanced) ==================================

	var trace []store.ToolInteraction
	var decisionIDs []string
	var statesVisited []string // PGPO (ADDED 2026-09-08)
	var previousState string   // PGPO (ADDED 2026-09-08)
	toolFailCount := make(map[string]int)

	maxTurns := s.calculateMaxTurns(req, role)

	var effectiveProviderID string
	var effectiveModelID string

	for turn := 0; turn < maxTurns; turn++ {
		// Abort if the task context was cancelled (e.g. CancelTask or shutdown).
		if ctx.Err() != nil {
			return s.failedOutput(role.BaseCapability, fmt.Sprintf("context cancelled: %v", ctx.Err())), nil
		}

		// VDA: Attach visual context from current state
		attachments := s.resolveContextAttachments(ctx, req.TaskID, req.StepID)

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

			// Dynamic Protocol: Re-build prompts and servers for each candidate family
			systemBlocks, err := s.buildSystemPrompt(req.Hub, role, prunedSkills, role.BaseCapability, pCfg, toolConstraints, mergedContext, toolFewShots, req.Isolation, precedents, req.Task, "")
			if err != nil {
				return s.failedOutput(role.BaseCapability, fmt.Sprintf("failed to render prompt: %v", err)), nil
			}
			mcpServers := s.buildMCPServers(ctx, req.Hub, req.TaskID, bindings, pCfg.Provider)

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
			forceTool := turn < maxTurns-1 && len(trace) == 0
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

			if os.Getenv("VORTEX_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "[DEBUG] Calling Provider %s for Task %s Turn %d (candidate %d)\n", pCfg.Model, req.TaskID, turn, candIdx)
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
					systemBlocks = squeezed.SystemBlocks
					userBlocks = squeezed.UserBlocks
					if squeezeRes.Decision == LevelCritical {
						cancel()
						return nil, fmt.Errorf("DYNAMIC_ESCALATION_REQUIRED: %s", squeezeRes.ActionTaken)
					}
				}
			}

			resp, err = provider.StreamComplete(ctxWithTimeout, providers.CompleteRequest{
				SystemBlocks:     systemBlocks,
				UserBlocks:       userBlocks,
				System:           flattenContentBlocks(systemBlocks),
				User:             flattenContentBlocks(userBlocks),
				Model:            pCfg.Model,
				MaxTokens:        4096,
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
					tail := string(patternWindow[len(patternWindow)-30:])
					if strings.Count(string(patternWindow), tail) >= 4 {
						return fmt.Errorf("ErrPatternLoopBrake")
					}
				}
				return nil
			})
			cancel()

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
					// No more candidates: fast-fail instead of looping
					s.logger.Log(EventStepFailed, req.TaskID, req.StepID, map[string]any{
						"error":   "empty_response_zero_tokens",
						"details": "all candidates returned 0 completion tokens; possible causes: vLLM missing --tool-call-parser, KV cache overflow, safety filter",
					})
					return nil, fmt.Errorf("empty_response_zero_tokens")
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
							advice := fmt.Sprintf("[ADVICE]: Encountered %q. Consider using these previously successful repair skills: %v", err.Error(), repairs)
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
			return nil, err
		}

		// Record Usage Stats
		GetGlobalStats().RecordUsage(currentPCfg.Provider, currentPCfg.Model, resp.PromptTokens, resp.CompletionTokens, len(resp.ToolCalls), resp.CacheWriteTokens, resp.CacheReadTokens)

		// ── Cost Governance: Cache efficiency sentinel (P0) ───────────────
		// Design ref: docs/architecture/COST_GOVERNANCE_ACTIVE_ALERT_AND_CONTROL.md §2.2 告警器1.
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

				s.Mu.Lock()
				if req.Hub.Graph.DecisionHistory == nil {
					req.Hub.Graph.DecisionHistory = make(map[string]*schemas.DecisionNode)
				}
				req.Hub.Graph.DecisionHistory[turnDecisionID] = node
				decisionIDs = append(decisionIDs, turnDecisionID)
				s.Mu.Unlock()

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

			// FIX (2026-09-07): Malformatted Tool Call Guard.
			// When the model outputs pseudo-XML like <anim:call> instead of real JSON tool calls,
			// intercept it and inject a stern correction prompt so it doesn't get trapped.
			if strings.Contains(cleanText, "<anim:call") || strings.Contains(cleanText, "<tool_call") {
				msg := "[SYSTEM ERROR]: You attempted to invoke a tool using invalid XML/pseudo-tags (e.g. <anim:call>). You MUST use the native JSON tool call structure provided by the platform. Please immediately issue the correct tool call in JSON format."
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
			if turn < maxTurns-1 && noToolCallsYet && (selfReportedLowConfidence || turn == 0) {
				msg := "[SYSTEM]: You have not executed any tools yet. Please proceed to actually call the necessary tool(s) now."
				if output.Status == schemas.StatusCapabilityRequired && len(output.RequiredCapabilities) > 0 {
					msg = fmt.Sprintf("[SYSTEM]: Before finalizing capability_required, double-check your bound tools. If none fulfill %v, explain and finalize.", output.RequiredCapabilities)
				}
				for i := range userBlocks {
					userBlocks[i].CacheControl = ""
				}
				userBlocks = append(userBlocks, schemas.ContentBlock{Text: cleanText})
				userBlocks = append(userBlocks, schemas.ContentBlock{Text: msg, CacheControl: "ephemeral"})
				continue
			}

			if noToolCallsYet && output.Status == schemas.StatusOK {
				output.Status = schemas.StatusPartial
				if output.Confidence > 0.4 {
					output.Confidence = 0.4
				}
				output.Warnings = append(output.Warnings, "orchestrator: model reported status=ok but no tool calls were recorded; downgraded to partial")
			}

			// ── Priority 5: Update Decision Outcome (Final) ──────────────────
			if turnDecisionID != "" && req.Hub != nil && req.Hub.Graph != nil {
				s.Mu.Lock()
				if node, ok := req.Hub.Graph.DecisionHistory[turnDecisionID]; ok {
					node.Outcome = string(output.Status)
				}
				s.Mu.Unlock()
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

		// ── Sieve Guardian: Tool Loop Detection ──────────────────────────────
		if valid, reason := s.checkSieveGuardian(req.TaskID, req.StepID, turn, resp.ToolCalls, trace); !valid {
			return s.failedOutput(role.BaseCapability, reason), nil
		}

		for _, call := range resp.ToolCalls {
			// Inject InputMapping (ADDED 2026-08-17)
			if call.Arguments == nil {
				call.Arguments = make(map[string]any)
			}
			for k, v := range req.InputMapping {
				if _, exists := call.Arguments[k]; !exists {
					call.Arguments[k] = v
				}
			}

			// Core tools (write_file, read_file, execute_code) — handled directly
			if CoreToolNames[call.Name] {
				taskDir := filepath.Join(s.outputBase, req.TaskID)
				validator := NewSafePathValidator(taskDir, req.SessionRoot, s.allowedWorkspaces)
				res, status, interaction := s.handleCoreToolCall(ctx, call, req.TaskID, req.StepID, validator)
				if status == "error" {
					toolFailCount[call.Name]++
					res = toolFailFeedback(call.Name, res, toolFailCount[call.Name])
				} else {
					toolFailCount[call.Name] = 0
				}
				trace = append(trace, interaction)
				env := s.processor.Wrap(call.Name, res, status, "orchestrator", "")
				toolResults = append(toolResults, env.ToMarkdown(call.Name))
				continue
			}

			mcpID := s.ResolveToolMCP(call.Name, bindings, req.Hub)
			mcpDef := req.Hub.GetMCP(mcpID)
			// Copy arguments so _decision_id stays in the trace only and
			// doesn't leak into the actual MCP tool call payload (fix M2).
			interaction := store.ToolInteraction{
				ToolName:  call.Name,
				Arguments: copyToolCallArguments(call.Arguments, turnDecisionID),
			}

			reason := "鏈煡 (LLM 鏈彁渚?"
			if r, ok := call.Arguments["_reason"].(string); ok && r != "" {
				reason = r
				delete(call.Arguments, "_reason")
			}

			if mcpDef == nil {
				toolFailCount[call.Name]++
				res := toolFailFeedback(call.Name, fmt.Sprintf("MCP server %s not found for tool %s", mcpID, call.Name), toolFailCount[call.Name])
				interaction.Result = res
				trace = append(trace, interaction)

				env := s.processor.Wrap(call.Name, res, "error", "orchestrator", reason)
				toolResults = append(toolResults, env.ToMarkdown(call.Name))
				continue
			}

			if isStateChangingTool(call.Name) {
				needsVerification = true
			}

			// Bookmark_List Routing: Inject trusted domains into search-like tools
			if strings.Contains(strings.ToLower(call.Name), "search") && len(req.Hub.Registry.System.Bookmarks) > 0 {
				if call.Arguments == nil {
					call.Arguments = make(map[string]any)
				}
				call.Arguments["_bookmarks"] = req.Hub.Registry.System.Bookmarks
			}

			if mcpDef.Command != "" {
				var finalRes any
				var execErr error

				// Tool Execution Timeout (default 120s)
				toolCtx, cancelTool := context.WithTimeout(ctx, 120*time.Second)

				// Helper to perform the actual call (including SAV verification)
				doCall := func() (any, error) {
					cli, err := s.mcpMgr.ensureMCPClient(toolCtx, mcpDef, req.TaskID)
					if err != nil {
						return nil, err
					}

					verifierID := req.Hub.Registry.GetVerifierID(call.Name)
					if verifierID != "" {
						if v, ok := s.verifierRegistry[verifierID]; ok {
							state, sErr := v.Sense(toolCtx, mcpID, call.Name, s)
							if sErr == nil {
								res, err := cli.SendRequest(toolCtx, "tools/call", map[string]any{
									"name":      call.Name,
									"arguments": call.Arguments,
								})
								if err != nil {
									return nil, err
								}
								success, vErr := v.Verify(toolCtx, mcpID, call.Name, state, res, s)
								if !success {
									return v.Recover(toolCtx, mcpID, call.Name, call.Arguments, vErr, s)
								}
								return res, nil
							}
							return nil, fmt.Errorf("SAV Sense failed: %v", sErr)
						}
					}

					return cli.SendRequest(toolCtx, "tools/call", map[string]any{
						"name":      call.Name,
						"arguments": call.Arguments,
					})
				}

				finalRes, execErr = doCall()

				// --- MCP Stdio Robustness: Auto-Reconnect Retry ---
				// If the process crashed or the pipe broke, evict from cache and retry once.
				if execErr != nil && client.IsConnectionError(execErr) {
					s.logger.Log("EventMCPReconnectAttempt", req.TaskID, req.StepID, map[string]any{
						"mcp":   mcpDef.ID,
						"tool":  call.Name,
						"error": execErr.Error(),
					})
					s.mcpMgr.EvictClient(mcpDef.ID)

					finalRes, execErr = doCall()
				}
				cancelTool()

				if execErr != nil {
					toolFailCount[call.Name]++
					resErr := toolFailFeedback(call.Name, fmt.Sprintf("Error executing tool %s: %v", call.Name, execErr), toolFailCount[call.Name])
					interaction.Result = resErr
					env := s.processor.Wrap(call.Name, resErr, "error", "mcp:"+mcpDef.ID, reason)
					toolResults = append(toolResults, env.ToMarkdown(call.Name))
				} else {
					toolFailCount[call.Name] = 0
					interaction.Result = finalRes
					if att, ok := finalRes.(schemas.SubagentOutput); ok {
						turnAttachments = append(turnAttachments, att.Attachments...)
					}
					status := "ok"
					if m, ok := finalRes.(map[string]any); ok {
						if s, ok := m["status"].(string); ok {
							status = s
						}
					}
					env := s.processor.Wrap(call.Name, finalRes, status, "mcp:"+mcpDef.ID, reason)
					md := env.ToMarkdown(call.Name)

					// Apply Sieve Pruning to reduce token noise for large tool outputs
					if len(md) > 2000 {
						md = s.sieve.PruneOutput(md, req.Task)
					}
					toolResults = append(toolResults, md)
				}
			} else if mcpDef.URL != "" {
				toolCtx, cancelTool := context.WithTimeout(ctx, 60*time.Second)
				finalRes, execErr := s.mcpMgr.callRemoteMCPTool(toolCtx, mcpDef, call.Name, call.Arguments)
				cancelTool()

				if execErr != nil {
					toolFailCount[call.Name]++
					resErr := toolFailFeedback(call.Name, fmt.Sprintf("Error executing remote tool %s: %v", call.Name, execErr), toolFailCount[call.Name])
					interaction.Result = resErr
					env := s.processor.Wrap(call.Name, resErr, "error", "mcp:"+mcpDef.ID, reason)
					toolResults = append(toolResults, env.ToMarkdown(call.Name))
				} else {
					toolFailCount[call.Name] = 0
					interaction.Result = finalRes
					status := "ok"
					if m, ok := finalRes.(map[string]any); ok {
						if s, ok := m["status"].(string); ok {
							status = s
						}
					}
					env := s.processor.Wrap(call.Name, finalRes, status, "mcp:"+mcpDef.ID, reason)
					md := env.ToMarkdown(call.Name)

					// Apply Sieve Pruning to reduce token noise for large tool outputs,
					// mirroring the local (command-based) MCP branch above.
					if len(md) > 2000 {
						md = s.sieve.PruneOutput(md, req.Task)
					}
					toolResults = append(toolResults, md)
				}
			} else {
				toolFailCount[call.Name]++
				res := toolFailFeedback(call.Name, fmt.Sprintf("MCP %s has neither a local command nor a remote URL configured for tool %s", mcpDef.ID, call.Name), toolFailCount[call.Name])
				interaction.Result = res
				env := s.processor.Wrap(call.Name, res, "error", "orchestrator:cloud", reason)
				toolResults = append(toolResults, env.ToMarkdown(call.Name))
			}
			trace = append(trace, interaction)

			// ── PGPO: State Observation Loop (ADDED 2026-09-08) ──
			currentState := s.GenerateStateSignature(call.Name, interaction.Result)
			statesVisited = append(statesVisited, currentState)
			if alert, delta := s.EvaluatePotentialDrop(previousState, currentState); alert {
				intervention := s.TriggerProactiveIntervention(currentState, req.Task)
				toolResults = append(toolResults, intervention)
				s.logger.Log("EventPotentialDropAlert", req.TaskID, req.StepID, map[string]any{
					"previous_state": previousState,
					"current_state":  currentState,
					"delta":          delta,
				})
			}
			previousState = currentState
		}

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
			s.Mu.Lock()
			if node, ok := req.Hub.Graph.DecisionHistory[turnDecisionID]; ok {
				node.Outcome = fmt.Sprintf("executed %d tools", len(resp.ToolCalls))
			}
			s.Mu.Unlock()
		}

		// Priority 4: Re-route tools for the next turn based on updated context
		// (Progressive Disclosure)
		bindings = s.toolRouter.Route(RouteRequest{
			Task:            req.Task + " " + resp.Text, // Include assistant thought in relevance check
			Bindings:        initialBindings,            // Re-evaluate from original set
			Turn:            turn + 1,
			ProgressiveMode: progressiveEnabled,
		})

		vdaPrompt := ""
		if needsVerification {
			vdaPrompt = "\n\n### VDA Verification Required\nState-changing tools were executed. A verification screenshot has been attached. Please analyze the image to verify if the UI state matches your expectation before proceeding."
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

		resMD := fmt.Sprintf("[ENVIRONMENT OBSERVATION]\n%s%s%s", observation, healingAdvice, vdaPrompt)
		userBlocks = append(userBlocks, schemas.ContentBlock{Text: resMD, CacheControl: "ephemeral"}) // Tool results turn
	}

	// FIX (2026-09-07): instead of hard-failing, emit a sentinel error the
	// scheduler converts into a resumable pending decision
	// (schemas.DecisionMaxTurnsExhausted). The step keeps its partial progress
	// (trace is persisted as the step's tool interactions) so a resume can
	// continue from here rather than restarting blind.
	return nil, fmt.Errorf("%s: tool execution loop reached max turns (%d) without a final answer; %d tool interaction(s) executed",
		ErrMaxTurnsExhausted, maxTurns, len(trace))
}
