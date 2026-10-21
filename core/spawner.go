package core

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/gateway"
	"github.com/daybeam/vortex/pkg/registry"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ── Package-level compiled regex (PB-3: avoid recompiling per turn) ────────
var thoughtRe = regexp.MustCompile("(?s)<thought>(.*?)</thought>")

// Feature flags are now read from Registry.System.FeatureFlags (hot-reloadable
// via StartWatcher). Env var overrides applied in applySystemDefaults.
// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md.

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
	sanitizer                *gateway.SanitizerProxy
	// ClearSanitizer clears the sanitizer vault for a completed task (P2).
	// Nil-safe: when nil or sanitizer is nil, call is a no-op.
	ClearSanitizer func(taskID string)

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
	// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §4.
	archive *ContextArchive

	// foldEngine (ADDED 2026-10-13): Segment-Based Incremental Fold engine.
	// Nil-safe: set via SetFoldEngine from DirectedEngine.
	// See docs/plan/SEGMENT_BASED_INCREMENTAL_FOLD_DESIGN.md.
	foldEngine *FoldEngine

	// effectTrackers (ADDED 2026-10-13): per-task EffectTracker for A2/A4
	// effect tracking. Keyed by taskID. Lazy-created on first access.
	// See docs/plan/EFFECT_BOUNDARY_DEFENSE_DESIGN.md §3.3.
	effectTrackers sync.Map // taskID → *EffectTracker

	// telemetry and providerRouter (ADDED 2026-10-21): provider routing telemetry
	// and strategy-based selection. Nil-safe: initialized in NewSpawner from config.
	// See docs/plan/PROVIDER_ROUTING_TELEMETRY_DESIGN.md.
	telemetry      *TelemetryRegistry
	providerRouter *ProviderRouter
}

func NewSpawner(reg *config.Registry, ts store.ITaskStore, es store.IExperienceStore, logger *Logger, loader *ResourceLoader, outputBase string) *Spawner {
	interceptors := []Interceptor{
		AuditInterceptor(logger), // FIRST: wrap everything in audit
		HealthCheckInterceptor(reg, logger),
		LogInterceptor(reg, logger, NewRoleGenerator(reg, loader, logger)),
		NewAntiSlopInterceptor(reg, logger).Wrap,
	}

	// Multi-language Governance Interceptors (ADDED 2026-07-14)
	for _, pc := range reg.System.ExternalInterceptors {
		interceptors = append(interceptors, NewExternalInterceptor(pc, reg.ExternalRuntimes, logger))
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
		processor:         NewOutputProcessor(getMaxToolResultChars(reg)),
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
	if reg.System.SanitizerEnabled {
		spawner.sanitizer = gateway.NewSanitizerProxy()
	}

	// Lazy discovery hook (2026-10-20): when LazyDiscovery is enabled,
	// Route calls this hook instead of expecting pre-populated AvailableTools.
	// The hook delegates to discoverStdioMCP via the MCP manager's per-ID lock.
	if reg.System.LazyDiscovery {
		spawner.toolRouter.SetLazyDiscoveryHook(func(ctx context.Context, mcpID, taskID string) error {
			if spawner.mcpMgr == nil {
				return nil
			}
			spawner.registry.Mu.RLock()
			mcp := spawner.registry.MCPs[mcpID]
			spawner.registry.Mu.RUnlock()
			if mcp == nil || mcp.Command == "" {
				return nil // remote MCPs handled elsewhere
			}
			dLock := spawner.mcpMgr.discoveryLock(mcpID)
			dLock.Lock()
			defer dLock.Unlock()
			spawner.discoverStdioMCP(ctx, mcp, taskID)
			return nil
		})
	}

	// ClearSanitizer clears the sanitizer vault for a completed task.
	// Called by DirectedEngine.finalize to prevent unbounded memory growth
	// and enforce ephemeral scope (P2 fix, 2026-10-18).
	spawner.ClearSanitizer = func(taskID string) {
		if spawner.sanitizer != nil {
			spawner.sanitizer.Clear(taskID)
		}
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

	// Initialize provider routing telemetry (ADDED 2026-10-21).
	// Nil-safe: if config is zero-value, defaults are applied in NewTelemetryRegistry.
	routingCfg := reg.System.ProviderRouting
	spawner.telemetry = NewTelemetryRegistry(routingCfg)
	spawner.providerRouter = NewProviderRouter(spawner.telemetry, routingCfg.Strategy)

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
// It also recomputes the OutputProcessor's MaxBodyChars from the model
// registry's MaxContextWindow when the user has NOT set an explicit
// MaxToolResultChars config (zero = "use dynamic default").
func (s *Spawner) SetModelRegistry(mr *registry.ModelRegistry) {
	s.modelRegistry = mr

	// If the user set an explicit MaxToolResultChars, respect it — don't override.
	// Otherwise, try to derive a dynamic default from the model registry.
	if mr == nil || s.registry == nil || s.processor == nil {
		return
	}
	s.registry.Mu.RLock()
	explicit := s.registry.System.MaxToolResultChars
	defaultProv := s.registry.DefaultProvider
	var pCfg *config.ProviderConfig
	if defaultProv != "" && s.registry.Providers != nil {
		pCfg = s.registry.Providers[defaultProv]
	}
	s.registry.Mu.RUnlock()

	if explicit > 0 {
		return // user override; leave processor as-is
	}
	if pCfg == nil || pCfg.Model == "" {
		return
	}

	// Use provider's explicit MaxContextWindow if set; otherwise query model registry.
	maxCtx := pCfg.MaxContextWindow
	if maxCtx <= 0 {
		maxCtx = mr.GetCapabilities(pCfg.Model).MaxContextWindow
	}
	if maxCtx > 0 {
		s.processor.SetMaxBodyChars(computeDynamicMaxBodyChars(maxCtx))
	}
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
// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.4.
func (s *Spawner) SetArchive(a *ContextArchive) {
	s.archive = a
	if s.pa != nil {
		s.pa.SetArchive(a)
	}
}

// SetFoldEngine injects the FoldEngine for segment-based incremental fold.
// Nil-safe: when nil, fold is skipped (zero behavior change).
// Also propagates to the PromptAssembler if it has been initialized.
// See docs/plan/SEGMENT_BASED_INCREMENTAL_FOLD_DESIGN.md.
func (s *Spawner) SetFoldEngine(fe *FoldEngine) {
	s.foldEngine = fe
	if s.pa != nil {
		s.pa.SetFoldEngine(fe)
	}
}

// SetMemoryBankStore wires the MemoryBankStore for UserProfile injection
// into sub-agent prompts. ADDED 2026-10-17.
// See docs/plan/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 3.
func (s *Spawner) SetMemoryBankStore(mb *store.MemoryBankStore) {
	if s.pa != nil {
		s.pa.SetMemoryBankStore(mb)
	}
}

// GetOrCreateEffectTracker returns the EffectTracker for a task, creating
// one lazily if it doesn't exist. Thread-safe via sync.Map.
// Used by the turnLoop to record effects (A2/A4) and by handleStepFailure
// to compensate effects on step failure (A3).
func (s *Spawner) GetOrCreateEffectTracker(taskID string) *EffectTracker {
	val, ok := s.effectTrackers.Load(taskID)
	if ok {
		return val.(*EffectTracker)
	}
	tracker := NewEffectTracker(filepath.Join(s.outputBase, taskID))
	actual, loaded := s.effectTrackers.LoadOrStore(taskID, tracker)
	if loaded {
		return actual.(*EffectTracker)
	}
	return tracker
}

// recordEffect is a convenience wrapper that records a tool call's effect
// on the task's EffectTracker. Called from the turnLoop after each tool call.
func (s *Spawner) recordEffect(taskID, stepID, toolName string, args map[string]any, result string, status string) {
	tracker := s.GetOrCreateEffectTracker(taskID)
	tracker.RecordEffect(taskID, stepID, toolName, args, result, status)
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
	ProviderID    string   // actual Provider ID used
	ModelID       string   // actual Model ID used
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
	// See docs/plan/STEP_PLAN_MODE_DESIGN.md §2.2.
	//
	// Auto-detection (§9): if EnableStepPlan is not explicitly set, check
	// StepPlanConfig.EnableProbe and run shouldEnableStepPlan (keyword +
	// optional System One probe) to decide automatically.
	//
	// RequireForComplexSteps (item #11): auto-enable for complex steps even
	// without EnableProbe — uses keyword heuristic only (zero cost).
	if !req.EnableStepPlan && s.registry != nil {
		sp := s.registry.StepPlan
		if sp.RequireForComplexSteps && schemas.DetermineComplexity(req.Task, 0) {
			req.EnableStepPlan = true
		} else if sp.EnableProbe {
			var probe providers.Provider
			if s.systemOneProvider != nil {
				probe = s.systemOneProvider
			}
			if s.shouldEnableStepPlan(req, probe) {
				req.EnableStepPlan = true
			}
		}
	}
	// eval E19 fix (2026-10-12): decider steps carry no Hub in their
	// SpawnRequest — planner would fail with "planner phase: Hub is nil".
	// Deciders decide skip/retry/abort; they never need an execution plan.
	if req.EnableStepPlan && req.Hub != nil {
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
