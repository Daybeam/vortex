package core

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/client"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/observability"
	"github.com/daybeam/vortex/pkg/registry"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ── Package-level compiled regex (PB-3: avoid recompiling per turn) ────────
var thoughtRe = regexp.MustCompile("(?s)<thought>(.*?)</thought>")

// ── Cached env reads (audit P-3: os.Getenv in hot path does syscall + map
// scan per call; cached once via sync.OnceValue for zero per-turn overhead.) ─
var orchestratorDebug = sync.OnceValue(func() bool { return os.Getenv("VORTEX_DEBUG") != "" })
var coreToolsOrderLast = sync.OnceValue(func() bool { return os.Getenv("VORTEX_CORE_TOOLS_ORDER") == "last" })

// audit P-MED-4: cache env vars read per-turn in the spawner loop.
var disableForceTool = sync.OnceValue(func() bool { return os.Getenv("ORCH_DISABLE_FORCE_TOOL") == "1" })
var evalObsEnabled = sync.OnceValue(func() bool { return os.Getenv("ORCH_EVAL_OBS") != "" })

// fsOnlyStreakLimitCache caches the ORCH_FS_ONLY_STREAK_LIMIT env var
// (audit P-3-residual: was read via os.Getenv once per Spawn call).
// Default 0 = disabled (metrics only).
var fsOnlyStreakLimitCache = sync.OnceValue(func() int {
	v := os.Getenv("ORCH_FS_ONLY_STREAK_LIMIT")
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return 0
})

// ToolCallReasonLLMInitiated is the default reason string for LLM-initiated
// tool calls. Extracted as a constant so tests can reference the production
// value instead of a local literal (fixes audit T-C03 / C-15).
const ToolCallReasonLLMInitiated = "tool call (LLM initiated)"

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
	domainReadCache   *DomainToolReadCache // #2-universal (eval §23): replay cached read results
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

	// signalCollector and policyDecider decouple data collection from policy
	// computation in calculateMaxTurns. See core/policy_signals.go and
	// core/policy_decider.go. Nil-safe: initialized in NewSpawner.
	signalCollector SignalCollector
	policyDecider   PolicyDecider

	// E1 fix: task-level tool fail count (persists across steps within a task).
	// The step-level toolFailCount (local var in doSpawn) resets per step;
	// this map persists across steps, so a tool failing in step 1, 2, and 3
	// accumulates to 3 and triggers toolFailFeedback's circuit-breaker.
	taskFailMu        sync.Mutex
	taskToolFailCount map[string]map[string]int // taskID → toolName → count

	// systemOneProvider (ADDED 2026-10-10): optional System One provider
	// for ProbeTaskComplexity auto-detection. Nil-safe: set via
	// SetSystemOneProvider on the DirectedEngine, which propagates to spawner.
	systemOneProvider *providers.SystemOneProvider

	// archive (ADDED 2026-10-10): ContextArchive for step summary queries.
	// Nil-safe: set via SetArchive from DirectedEngine.
	// See docs/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §4.
	archive *ContextArchive
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
		domainReadCache:   NewDomainToolReadCache(), // #2-universal: replay cached read results
		watchdog:          NewGatewayWatchdog(reg),
		healer:            NewHealer(),
		pa:                NewPromptAssembler(es, loader, reg, logger),
		irtEstimator:      NewIRTBudgetEstimator(50),
		cacheSentinel:     NewCacheSentinel(),
		allowedWorkspaces: reg.System.Sandbox.AllowedWorkspaces,
	}

	// Wire policy signal collection and decision (decoupled from spawner_policy.go)
	spawner.signalCollector = NewStoreBackedSignalCollector(es, nil, spawner.irtEstimator)
	spawner.policyDecider = NewIRTPolicyDecider(spawner.irtEstimator, reg)

	// Priority 6: Wire up embedding client for Experience Store
	if es != nil {
		hub := NewContextHub(reg, nil, es)
		if esStore, ok := es.(*store.ExperienceStore); ok {
			esStore.SetEmbeddingClient(hub.GetEmbeddingClient())
		}

	// Wire up embedding client for ToolRouter hybrid search.
	spawner.toolRouter.SetEmbedClient(NewHubEmbeddingClient(hub))
	refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 30*time.Second) // audit L-MED-6: add timeout to avoid blocking watcher indefinitely
	spawner.toolRouter.RefreshToolEmbeddings(refreshCtx, reg)
	refreshCancel()
	reg.OnReload(func() {
		reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 30*time.Second) // audit L-MED-6: add timeout to avoid blocking watcher indefinitely
		defer reloadCancel()
		spawner.toolRouter.RefreshToolEmbeddings(reloadCtx, reg)
	})
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
	// Re-wire signal collector now that capProfileStore is available
	s.signalCollector = NewStoreBackedSignalCollector(s.expStore, cps, s.irtEstimator)
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

// SetArchive injects the ContextArchive for Path D (Regular Step Warm layer).
// Nil-safe: when nil, Archive queries are skipped (zero behavior change).
// Also propagates to the PromptAssembler if it has been initialized.
// See docs/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.4.
func (s *Spawner) SetArchive(a *ContextArchive) {
	s.archive = a
	if s.pa != nil {
		s.pa.SetArchive(a)
	}
}

// RecordSubagentDuration records the last subagent execution duration for the
// P2-11 feedback loop (auto-tighten warm context budget on high latency).
// Propagates to the PromptAssembler. Nil-safe.
func (s *Spawner) RecordSubagentDuration(d time.Duration) {
	if s.pa != nil {
		s.pa.RecordSubagentDuration(d)
	}
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

	// Step Plan Mode (ADDED 2026-10-10): if enabled, run a Planner Phase
	// before the interceptor chain. The plan is injected into
	// AdditionalPromptContext so the executor sees it.
	// See docs/STEP_PLAN_MODE_DESIGN.md §2.2.
	//
	// Auto-detection (§9): if EnableStepPlan is not explicitly set, check
	// StepPlanConfig.EnableProbe and run shouldEnableStepPlan (keyword +
	// optional System One probe) to decide automatically.
	if !req.EnableStepPlan && s.registry != nil && s.registry.StepPlan.EnableProbe {
		var probe providers.Provider
		if s.systemOneProvider != nil {
			probe = s.systemOneProvider
		}
		if s.shouldEnableStepPlan(req, probe) {
			req.EnableStepPlan = true
		}
	}
	if req.EnableStepPlan {
		plan, planErr := s.runPlannerPhase(ctx, req)
		if planErr != nil {
			s.logger.Log("step_plan_failed", req.TaskID, req.StepID, map[string]any{"error": planErr.Error()})
			// Non-fatal: fall through to normal execution without a plan.
			// The step can still succeed; the plan is an enhancement.
		} else {
			s.logger.Log("step_plan_generated", req.TaskID, req.StepID, map[string]any{"plan_len": len(plan)})
			planHeader := "## Step Plan (generated by Planner Phase)\n\n" + plan + "\n\n## Execution\nFollow the plan above. Each sub-action should be executed in order.\n\n"
			req.AdditionalPromptContext = planHeader + req.AdditionalPromptContext
			// Store plan in Sieve for plan-drift detection (#4 + #10).
			if s.sieve != nil {
				s.sieve.SetPlan(req.TaskID, plan)
			}
		}
	}

	handler := BuildInterceptorChain(s.interceptors, s.doSpawn)
	start := time.Now()
	result, err := handler(ctx, req)
	// audit L-N12 (fix-propagation gap, 4th occurrence): the interceptor chain
	// can short-circuit and return (nil, nil). Previously only spawnDecider
	// (L-N10) guarded against this; 7 sibling call sites did not, causing nil
	// pointer dereferences on res.Output.Result. Root-cause fix: ensure Spawn
	// NEVER returns (nil, nil) so all callers can safely dereference.
	if err == nil && result == nil {
		result = &SpawnResult{Output: schemas.SubagentOutput{Result: map[string]any{}}}
	}
	if err == nil && result != nil {
		s.recordCapabilityOutcome(ctx, req, result, time.Since(start))
	}
	// P2-11: Feed subagent duration into the warm budget feedback loop.
	s.RecordSubagentDuration(time.Since(start))
	return result, err
}

// shouldRetryEmptyResponse increments the empty-response retry counter and
// decides whether the caller should retry the turn (returns true, nil) or
// fail fast (returns false, err). The error is non-nil only when retries
// are exhausted.
//
// Regression: eval §10.4 — Xunfei MaaS returns intermittent empty responses
// (~40% of requests). Without retry, a single empty response kills the step.
// With maxEmptyRespRetries=2, the effective failure rate drops to ~6.4%.
func shouldRetryEmptyResponse(retries *int, max int) (bool, error) {
	*retries++
	if *retries <= max {
		return true, nil
	}
	return false, fmt.Errorf("empty_response_zero_tokens: exhausted %d retries", max)
}

// doSpawn is the core spawning logic, wrapped by interceptors.

// doSpawn is the core spawning logic, wrapped by interceptors.
func (s *Spawner) doSpawn(ctx context.Context, req *SpawnRequest) (*SpawnResult, error) {
	// Ensure Hub exists (safety fallback)
	if req.Hub == nil {
		req.Hub = NewContextHub(s.registry, nil, s.expStore)
	}
	// P-2.1 Step 2: GraphMu fallback removed — DecisionHistory access now goes
	// through TaskGraph.RecordDecision/UpdateDecisionOutcome (nil-safe on Graph).

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
		bindings = s.toolRouter.Route(ctx, RouteRequest{
			Task:               req.Task,
			Bindings:           bindings,
			Turn:               0,
			ProgressiveMode:    progressiveEnabled,
			PredecessorSkillID: req.PredecessorSkillID,
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
		userText = fmt.Sprintf("%s\n\n%s\n%s", userText, MarkerRecoveryContext, req.AdditionalPromptContext)
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
	// #11 (eval §8.41): Per-task identical-call counter for dedup.
	// Key = toolName + ":" + fnv(args). Allows 2 identical calls; the 3rd
	// requires a _reason argument (legitimate retries like data refresh pass).
	// Per-step scope: different steps may legitimately make the same call
	// (e.g., parallel searches gathering information). Task-level scope would
	// block normal cross-step work.
	toolCallCount := make(map[string]int) // callKey → count (resets per step)
	sieveGuardianHits := 0
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
	// Default 0 = disabled (metrics only). Set ORCH_FS_ONLY_STREAK_LIMIT=5
	// to enable. Start conservative to avoid aborting legitimate long
	// reasoning chains that use fs-only tools productively.
	// audit P-3-residual: cached via fsOnlyStreakLimitCache (sync.OnceValue).
	fsOnlyStreakLimit := fsOnlyStreakLimitCache()

	maxTurns := s.calculateMaxTurns(ctx, req, role)
	// applyStrategyBias is now handled inside calculateMaxTurns via PolicyDecider

	var effectiveProviderID string
	var effectiveModelID string

	// Empty response retry: retry the turn when the provider returns HTTP 200
	// but zero tokens (intermittent server-side empty response, e.g. Xunfei MaaS).
	// See eval §10.4: ~40% of Xunfei responses had non-zero usage but empty content.
	const maxEmptyRespRetries = 2
	emptyRespRetries := 0

turnLoop:
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
			systemBlocks, err := s.buildSystemPrompt(ctx, req.Hub, role, prunedSkills, role.BaseCapability, pCfg, toolConstraints, mergedContext, toolFewShots, req.Isolation, precedents, req.Task, "", req.TaskID)
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
			forceTool := turn < maxTurns-1 && len(trace) == 0 && !disableForceTool()
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

			if orchestratorDebug() {
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
						"error":  "empty_response_zero_tokens",
						"retry":  emptyRespRetries,
						"max":    maxEmptyRespRetries,
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

			// [EVAL-OBS] 2026-10-07 观测打印：定位「一个 τ² turn 内引擎到底跑了几轮、
			// 每轮调了什么、为什么没退出」。由 ORCH_EVAL_OBS 环境变量开启，默认关闭。
			if evalObsEnabled() {
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
			// When the model outputs pseudo-XML like <anim:call> instead of real JSON tool calls,
			// intercept it and inject a stern correction prompt so it doesn't get trapped.
			if strings.Contains(cleanText, "<anim:call") || strings.Contains(cleanText, "<tool_call") {
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
			if turn < maxTurns-1 && noToolCallsYet && (selfReportedLowConfidence || turn == 0) && !disableForceTool() {
				if evalObsEnabled() {
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

			if noToolCallsYet && output.Status == schemas.StatusOK {
				output.Status = schemas.StatusPartial
				if output.Confidence > 0.4 {
					output.Confidence = 0.4
				}
				output.Warnings = append(output.Warnings, "orchestrator: model reported status=ok but no tool calls were recorded; downgraded to partial")
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
					MaxTokens: 4096,
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

		for _, call := range resp.ToolCalls {
			// #2+#14 (eval §8.34): Side-effect classification. Core tools
			// (write_file/read_file/execute_code) are fs-only — no domain
			// side-effect. Domain MCP tools have real side-effects. Pure
			// metrics, zero behavior change.
			if CoreToolNames[call.Name] {
				observability.GetGlobalMetrics().Inc("orchestrator.calls.fs_only", 1)
			} else {
				observability.GetGlobalMetrics().Inc("orchestrator.calls.domain", 1)
			}

			// Inject InputMapping (ADDED 2026-08-17)
			if call.Arguments == nil {
				call.Arguments = make(map[string]any)
			}
			for k, v := range req.InputMapping {
				if _, exists := call.Arguments[k]; !exists {
					call.Arguments[k] = v
				}
			}

			// #11 (eval §8.41): Identical-call dedup. Allow 2 identical
			// calls (same tool + same args); on the 3rd, require a _reason
			// argument. Legitimate retries (data refresh, waiting for async
			// result) pass by providing _reason. Mindless repetition is
			// blocked, saving turn budget.
			// Per-step scope: resets each step. Different steps may legitimately
			// make the same call (parallel searches, phased gathering).
			callKey := toolCallKey(call.Name, call.Arguments)
			callCount := toolCallCount[callKey]
			if callCount >= 2 {
				reason, hasReason := call.Arguments["_reason"].(string)
				if !hasReason || reason == "" {
					observability.GetGlobalMetrics().Inc("orchestrator.dedup.blocked", 1)
					toolResults = append(toolResults, fmt.Sprintf(
						"[%s] This is the %drd identical call with the same arguments. "+
							"Previous results were identical. If this is a legitimate retry "+
							"(e.g., waiting for data refresh), provide a `_reason` argument "+
							"explaining why. Otherwise, use the previous result.",
						call.Name, callCount+1))
					continue
				}
				// Legitimate retry with reason — allow and log.
				observability.GetGlobalMetrics().Inc("orchestrator.dedup.reasoned_retry", 1)
				s.logger.Log("EventDedupReasonedRetry", req.TaskID, req.StepID, map[string]any{
					"tool":   call.Name,
					"count":  callCount + 1,
					"reason": reason,
				})
			}
			toolCallCount[callKey]++

			// Core tools (write_file, read_file, execute_code) — handled directly
			if CoreToolNames[call.Name] {
				taskDir := filepath.Join(s.outputBase, req.TaskID)
				validator := NewSafePathValidator(taskDir, req.SessionRoot, s.allowedWorkspaces)
				res, status, interaction := s.handleCoreToolCall(ctx, call, req.TaskID, req.StepID, validator)
				if status == "error" {
					toolFailCount[call.Name]++
					effCount := s.incrTaskToolFail(req.TaskID, call.Name, toolFailCount[call.Name])
					res = toolFailFeedback(call.Name, res, effCount)
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

			reason := ToolCallReasonLLMInitiated // audit C-15: replaced mojibake string literal
			if r, ok := call.Arguments["_reason"].(string); ok && r != "" {
				reason = r
				delete(call.Arguments, "_reason")
			}

			// #19 (eval §8.42): Write reflection gate. MCP tools that modify
			// external state (cancel_*, update_*, create_*, etc.) require a
			// _reason argument. The purpose is to nudge the model to reflect
			// on the current situation before writing: "What is the state?
			// Should I change strategy? Is this call necessary?"
			// If _reason is provided, the call proceeds — the model reflected
			// and decided to act. If not, the reflection prompt is returned.
			// StagedWorkspace provides rollback for file operations; this gate
			// provides accountability for MCP operations that can't be rolled back.
			if IsWriteMCPTool(call.Name) && reason == ToolCallReasonLLMInitiated {
				// 🔴 audit DEAD-CODE (2026-10-04): the branch below could never
				// fire. Reason was already consumed at :1151-1154, which does
				// `delete(call.Arguments, "_reason")`; the type assertion here
				// re-reads the SAME (now deleted) key, so hasReason was always
				// false and EventWriteAuthAllowed was never emitted. The gate
				// actually worked via the `reason` copy on the outer condition.
				// Kept (not deleted) so the two paths can be distinguished again
				// once the reason is captured before deletion.
				if writeReason, hasReason := call.Arguments["_reason"].(string); hasReason && writeReason != "" {
					// Model reflected and provided a reason — allow the write.
					s.logger.Log("EventWriteAuthAllowed", req.TaskID, req.StepID, map[string]any{
						"tool":   call.Name,
						"mcp":    mcpID,
						"reason": writeReason,
					})
				} else {
					observability.GetGlobalMetrics().Inc("orchestrator.auth.write_blocked", 1)
					s.logger.Log("EventWriteAuthBlocked", req.TaskID, req.StepID, map[string]any{
						"tool": call.Name,
						"mcp":  mcpID,
					})
					// 2026-10-04 (eval §8.51.4-§8.51.7, four prompt rounds measured):
					//
					// R1 prose + fragmentary example — model complied 18 times, all
					//    rejected; it pasted the fragment into another argument's
					//    VALUE with full-width quotes: {"reservation_id": "XEHM4B””, “_reason": "}
					// R2 full example + two WRONG counter-examples — quotes fixed,
					//    but it copied a counter-example: {"reservation_id": "XEHM4B", "}
					// R3 ONE example, no counter-examples — full-width quotes came BACK,
					//    so the counter-examples were never the cause.
					//
					// R4 (this one) — prompt, don't gate. The block comment above
					// states the purpose is to NUDGE reflection ("What is the
					// state? Should I change strategy? Is this call necessary?").
					// It is not an authorization check — the engine has no way to
					// know whether the customer consented. Three prompt rounds
					// failed to make a 4B model satisfy the format, and the cost
					// of continuing to block is that the write NEVER happens
					// (airline task 7: Write Actions 0/3 across all four arms).
					// So: keep the nudge in the tool result, record the intent,
					// and let the call proceed. Guidance preserved, veto removed.
					// Set system.require_write_reason=true to restore hard blocking.
					nudge := fmt.Sprintf(
						"[%s] This call changes external state. Before you rely on it, note: "+
							"what is the current state, and is this write necessary? "+
							"Proceeding without a `_reason`.",
						call.Name)
					interaction.Result = nudge
					trace = append(trace, interaction)
					if s.registry != nil && s.registry.System.RequireWriteReason {
						nudge = fmt.Sprintf(
							"[%s] AUTHORIZATION REQUIRED — this call changes external state.\n"+
								"Add a top-level key named \"_reason\" whose value is one short sentence.\n"+
								"Call it again with this exact shape:\n"+
								"  {\"reservation_id\": \"<the id>\", \"_reason\": \"<why this write is correct>\"}",
							call.Name)
						interaction.Result = nudge
						env := s.processor.Wrap(call.Name, nudge, "error", "orchestrator", reason)
						toolResults = append(toolResults, env.ToMarkdown(call.Name))
						continue
					}
					s.logger.Log("EventWriteAuthNudged", req.TaskID, req.StepID, map[string]any{
						"tool": call.Name,
						"mcp":  mcpID,
					})
				}
			}

			if mcpDef == nil {
				toolFailCount[call.Name]++
				effCount := s.incrTaskToolFail(req.TaskID, call.Name, toolFailCount[call.Name])
				res := toolFailFeedback(call.Name, fmt.Sprintf("MCP server %s not found for tool %s", mcpID, call.Name), effCount)
				interaction.Result = res
				trace = append(trace, interaction)

				env := s.processor.Wrap(call.Name, res, "error", "orchestrator", reason)
				toolResults = append(toolResults, env.ToMarkdown(call.Name))
				continue
			}

		if isStateChangingTool(call.Name) {
			needsVerification = true
		}

		// #2-universal (eval §23): a state-changing (write) call invalidates
		// every cached read for this MCP server, keeping the read cache
		// coherent with external state. Generic: any write verb prefix
		// (cancel_/update_/create_/...) or known state-changing tool.
		if s.domainReadCache != nil && (IsWriteMCPTool(call.Name) || isStateChangingTool(call.Name)) {
			s.domainReadCache.Invalidate(mcpID)
		}

		// Bookmark_List Routing: Inject trusted domains into search-like tools
		if strings.Contains(strings.ToLower(call.Name), "search") && len(req.Hub.Registry.System.Bookmarks) > 0 {
			if call.Arguments == nil {
				call.Arguments = make(map[string]any)
			}
			call.Arguments["_bookmarks"] = req.Hub.Registry.System.Bookmarks
		}

		// #2-universal (eval §23): replay cached read results to break
		// repetition loops (execution amplification) without force-terminating
		// the conversation. Generic: any read-only domain tool (get_/list_/
		// search_/query_/read_/fetch_/check_/is_/has_*) is eligible; pollers
		// (progress/health/wait) and state-changing tools are excluded. The
		// model receives the identical previously-computed markdown, so the
		// repeated call costs zero extra tool execution.
		if s.domainReadCache != nil && isReadTool(call.Name) && !isPollerTool(call.Name) {
			if cachedMD, ok := s.domainReadCache.Get(mcpID, call.Name, call.Arguments); ok {
				log.Printf("[spawner] read_cache HIT task=%s step=%s tool=%s mcp=%s", req.TaskID, req.StepID, call.Name, mcpID)
				observability.GetGlobalMetrics().Inc("orchestrator.read_cache.hit", 1)
				interaction.Result = cachedMD
				trace = append(trace, interaction)
				toolResults = append(toolResults, cachedMD)
				continue
			}
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
					effCount := s.incrTaskToolFail(req.TaskID, call.Name, toolFailCount[call.Name])
					resErr := toolFailFeedback(call.Name, fmt.Sprintf("Error executing tool %s: %v", call.Name, execErr), effCount)
					interaction.Result = resErr
					env := s.processor.Wrap(call.Name, resErr, "error", "mcp:"+mcpDef.ID, reason)
					toolResults = append(toolResults, env.ToMarkdown(call.Name))
				} else {
					// SERF: check MCP isError field before treating as success (ADDED 2026-10-03)
					isToolError, serf, errMsg := parseMCPError(finalRes)
					if isToolError {
						toolFailCount[call.Name]++
						effCount := s.incrTaskToolFail(req.TaskID, call.Name, toolFailCount[call.Name])
						recoveryMsg := buildSERFRecovery(call.Name, serf, errMsg, effCount)
						interaction.Result = recoveryMsg
						env := s.processor.Wrap(call.Name, recoveryMsg, "error", "mcp:"+mcpDef.ID, reason)
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
					// #2-universal (eval §23): cache the successful read result
					// for replay on identical future calls within this server.
					if s.domainReadCache != nil && isReadTool(call.Name) && !isPollerTool(call.Name) {
						s.domainReadCache.Put(mcpID, call.Name, call.Arguments, md)
					}
				}
				}
			} else if mcpDef.URL != "" {
				toolCtx, cancelTool := context.WithTimeout(ctx, 60*time.Second)
				finalRes, execErr := s.mcpMgr.callRemoteMCPTool(toolCtx, mcpDef, call.Name, call.Arguments)
				cancelTool()

				if execErr != nil {
					toolFailCount[call.Name]++
					effCount := s.incrTaskToolFail(req.TaskID, call.Name, toolFailCount[call.Name])
					resErr := toolFailFeedback(call.Name, fmt.Sprintf("Error executing remote tool %s: %v", call.Name, execErr), effCount)
					interaction.Result = resErr
					env := s.processor.Wrap(call.Name, resErr, "error", "mcp:"+mcpDef.ID, reason)
					toolResults = append(toolResults, env.ToMarkdown(call.Name))
				} else {
					// SERF: check MCP isError field before treating as success (ADDED 2026-10-03)
					isToolError, serf, errMsg := parseMCPError(finalRes)
					if isToolError {
						toolFailCount[call.Name]++
						effCount := s.incrTaskToolFail(req.TaskID, call.Name, toolFailCount[call.Name])
						recoveryMsg := buildSERFRecovery(call.Name, serf, errMsg, effCount)
						interaction.Result = recoveryMsg
						env := s.processor.Wrap(call.Name, recoveryMsg, "error", "mcp:"+mcpDef.ID, reason)
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
						// #2-universal (eval §23): cache the successful read result.
						if s.domainReadCache != nil && isReadTool(call.Name) && !isPollerTool(call.Name) {
							s.domainReadCache.Put(mcpID, call.Name, call.Arguments, md)
						}
					}
				}
			} else {
				toolFailCount[call.Name]++
				effCount := s.incrTaskToolFail(req.TaskID, call.Name, toolFailCount[call.Name])
				res := toolFailFeedback(call.Name, fmt.Sprintf("MCP %s has neither a local command nor a remote URL configured for tool %s", mcpDef.ID, call.Name), effCount)
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
			req.Hub.Graph.UpdateDecisionOutcome(turnDecisionID, fmt.Sprintf("executed %d tools", len(resp.ToolCalls)))
		}

		// Priority 4: Re-route tools for the next turn based on updated context
		// (Progressive Disclosure)
		bindings = s.toolRouter.Route(ctx, RouteRequest{
			Task:               req.Task + " " + resp.Text,
			Bindings:           initialBindings,
			Turn:               turn + 1,
			ProgressiveMode:    progressiveEnabled,
			PredecessorSkillID: req.PredecessorSkillID,
		})

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
