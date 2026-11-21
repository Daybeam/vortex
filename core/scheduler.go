package core

import (
	"github.com/daybeam/vortex/core/sieve"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/jit"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/bootmetrics"
	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/pkg/registry"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// recordHashCacheEntry stores a hash in the cache with eviction. Extracted so
// tests can call the real production path instead of inlining the logic
// (fixes audit T-C04). Safe to clear on eviction because the cache is only
// an optimization — cleared entries will be re-hashed on next access.
func (s *DirectedEngine) recordHashCacheEntry(path, hash string) {
	s.hashCacheMu.Lock()
	defer s.hashCacheMu.Unlock()
	if s.hashCache == nil {
		s.hashCache = make(map[string]string)
	}
	if len(s.hashCache) >= s.maxHashCacheEntries {
		// audit PERF-4: was clearing ALL entries (cliff-edge → thundering herd
		// of re-hashing on next persist). Keep half to spread the re-hash cost
		// across subsequent calls instead of one burst. Map iteration order is
		// randomized in Go, so this keeps a random half — no LRU dependency needed.
		keep := make(map[string]string, len(s.hashCache)/2)
		for k, v := range s.hashCache {
			keep[k] = v
			if len(keep) >= len(s.hashCache)/2 {
				break
			}
		}
		s.hashCache = keep
	}
	s.hashCache[path] = hash
}

// DirectedEngine owns all task graphs and drives execution.
// Go advantage: each task graph runs in its own goroutine.
// No asyncio event loop — the language runtime handles concurrency.
type DirectedEngine struct {
	Mu          sync.RWMutex
	graphs      map[string]*schemas.TaskGraph
	cancelFuncs map[string]context.CancelFunc
	doneChans   map[string]chan struct{} // For long-polling
	// stepWGs tracks in-flight step goroutines per task so CancelTask/Clear
	// can wait for them to drain before proceeding (L-7.3). Without this,
	// a late persistGraph from a draining step can re-create a zombie task
	// in the registry after Clear has already deleted it.
	stepWGs map[string]*sync.WaitGroup

	// H-5 (2026-10-04): Debounce map for persistGraph — taskID → last persist
	// time. Skips redundant persists within 200ms, eliminating the 3-4× per
	// step lifecycle marshal overhead. sync.Map for lock-free access.
	lastPersistAt sync.Map

	// The graph persist fence (register O33, decision Q2-2). Semantics and its
	// two known limits are in core/scheduler_fence.go.
	fenceOwner string
	fences     sync.Map // taskID → graphFence: the token this process last claimed
	// fenceCancels holds each executor's own cancel func so losing the fence can
	// stop the goroutine, not only its writes (a muted executor still burns
	// tokens and still fires external MCP calls).
	fenceCancels sync.Map // taskID → context.CancelFunc
	fenceBroken  sync.Map // taskID → struct{}: sticky refuse, cleared only by a fresh claim

	spawner        *Spawner
	jit            *jit.JITManager
	taskStore      store.ITaskStore
	expStore       store.IExperienceStore
	reflection     *ReflectionEngine
	logger         *obs.Logger
	assets         *AssetManager
	registry       *config.Registry
	outputBase     string
	tmpBase        string
	SignalField    *SignalField
	sieve          *sieve.Sieve
	budgetGuard    *BudgetGuard
	budgetSentinel *BudgetSentinel
	walker         *DAGGraphWalker
	drafts         *DraftStore

	// JITSessions holds POC session-scoped JIT interpreter state (design doc
	// playbook/jit-session-poc-design-2026.md). Nil-safe: set explicitly by
	// main.go/main_web.go after NewDirectedEngine returns, rather than
	// threaded through the constructor, to avoid breaking the 4 existing
	// test call sites that construct a DirectedEngine directly. executeStep
	// checks for nil before use.
	JITSessions *jit.JITSessionManager

	// TaskRegistry persists task-level lifecycle state so GetStatus() can answer
	// for graphs that are absent from the in-process map — which happens under the
	// Master/Proxy split and after a restart, where the submitting process and the
	// querying process are not the same one.
	// Nil-safe: set explicitly by main*.go after NewDirectedEngine returns, same
	// rationale as JITSessions above. All call sites must nil-check before use.
	// Guarded by taskRegistryMu: the Asset Sentry goroutine (started below, in
	// this constructor) reads it while main*.go assigns it. Use
	// taskRegistry()/SetTaskRegistry instead of touching the field directly.
	TaskRegistry store.TaskRegistry
	taskRegistryMu sync.RWMutex

	// BaselineStore pins SOP/Role versions at task creation time so resumed
	// tasks use the pinned version instead of the live registry. Nil-safe:
	// set explicitly by main*.go after NewDirectedEngine returns. When nil,
	// task creation skips pinning and resume falls back to current registry.
	BaselineStore store.BaselineStore

	// Archive is the hot-data memory layer (ContextArchive). Nil-safe: set
	// explicitly by main*.go after NewDirectedEngine returns. When non-nil,
	// handleOutput ingests a MemoryItem after each successful step completion.
	Archive *ContextArchive

	// Sessions tracks session-to-workspace bindings for sandbox enforcement
	// (D1). Nil-safe: set explicitly by main*.go after NewDirectedEngine
	// returns. When non-nil, SubmitWithSessionIR validates workspaceRoot
	// against the AllowedWorkspaces whitelist before accepting the task.
	// When nil (tests, backward compat), the check is skipped.
	Sessions *SessionManager

	// pathLocks (ADDED 2026-09-13) — fine-grained per-path RWMutex for
	// concurrent step file-write isolation. See core/path_lock.go.
	pathLocks *PathLockManager

	UseSwarm bool

	// systemOneProvider is the optional System One decision model provider.
	// Nil-safe: set explicitly by main*.go after NewDirectedEngine returns.
	// When non-nil, decision gate, tool router, and failure classification
	// use it for fast probability-based decisions instead of expensive LLM
	// calls or string matching. Each integration point nil-checks before use.
	systemOneProvider *providers.SystemOneProvider
	systemOneCfg      config.SystemOneConfig
	notifyChan        chan struct{}
	notifyMu          sync.Mutex

	// lifecycleCtx is cancelled by Stop() to shut down background services
	// and async poll goroutines. Replaces context.Background() at
	// service-start sites.
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	// bgWg tracks fire-and-forget background goroutines (embedding, folding,
	// reflection) so Stop() can wait for them to drain before returning.
	bgWg sync.WaitGroup

	notifMu       sync.RWMutex
	notifications []NotificationRecord

	// hashCache caches SHA-256 hashes of output files to avoid re-hashing
	// on every persistGraph call. Keyed by file path. persistGraph is invoked
	// from concurrent step goroutines (executeStep runs up to 10 steps of one
	// graph in parallel), so the cache is guarded by a dedicated mutex rather
	// than Mu — Mu is not held at every persistGraph call site (e.g. the
	// post-status-update checkpoint in executeStep), and nesting under Mu here
	// would risk deadlock with callers that already hold it. The hash compute
	// itself (io.Copy) runs OUTSIDE the lock so I/O never serializes across
	// distinct paths.
	hashCache   map[string]string
	hashCacheMu sync.Mutex

	// maxHashCacheEntries caps the hashCache size to prevent unbounded memory
	// growth on long-running servers. When exceeded, the cache is cleared
	// (safe — it is only an optimization to avoid re-hashing write-once files).
	maxHashCacheEntries int

	// hookRunner executes config-driven automated hooks at DAG lifecycle points
	// (step_post, task_completion, decision_required). Nil-safe: set by
	// WireHooks after construction. See core/hook_runner.go.
	hookRunner *HookRunner

	// decSem bounds concurrent NonInteractive decision-resolution goroutines
	// to prevent unbounded LLM HTTP calls when many steps block in parallel.
	// (audit NEW-P1: mirrors the crystSem pattern in reflection.go:307.)
	decSem chan struct{}

	// staleTaskSnapshots tracks the last known progress state of each running
	// task for the stale-task auto-fail monitor. Keyed by taskID. Only
	// accessed from the monitor's background goroutine while holding s.Mu.
	staleTaskSnapshots map[string]*staleTaskState

	// recentDecisions (#14, ADDED 2026-10-10): per-step decision timestamp
	// for RiskTier-based rate limiting. Outer key: taskID, inner key: stepID.
	// See docs/plan/STEP_PLAN_MODE_DESIGN.md §13.3.
	recentDecisions   map[string]map[string]time.Time
	recentDecisionsMu sync.Mutex

	// auditLogger (#15, ADDED 2026-10-11): tamper-evident SHA-256 hash chain
	// for Decision Gate events. Nil-safe: set by main*.go after construction.
	// See docs/plan/STEP_PLAN_MODE_DESIGN.md §13.4.
	auditLogger *AuditLogger

	// costTracker (#18, ADDED 2026-10-15): per-LLM-call token spend to SQLite.
	// Nil-safe: set by main*.go after construction.
	// See docs/plan/STEP_PLAN_MODE_DESIGN.md §13.7.
	costTracker *CostTracker
}

const defaultMaxHashCacheEntries = 10000

type NotificationRecord struct {
	TaskID    string    `json:"task_id"`
	Status    string    `json:"status"`
	Event     string    `json:"event"`
	Timestamp time.Time `json:"timestamp"`
	Read      bool      `json:"read"`
}

var _ OrchestrationEngine = (*DirectedEngine)(nil)

var _ interfaces.TaskHubInterface = (*DirectedEngine)(nil)

func NewDirectedEngine(
	reg *config.Registry,
	ts store.ITaskStore,
	es store.IExperienceStore,
	jit *jit.JITManager,
	logger *obs.Logger,
	sf *SignalField,
	outputBase, tmpBase string,
	loader *ResourceLoader,
) *DirectedEngine {
	s := &DirectedEngine{
		graphs:              make(map[string]*schemas.TaskGraph),
		cancelFuncs:         make(map[string]context.CancelFunc),
		doneChans:           make(map[string]chan struct{}),
		stepWGs:             make(map[string]*sync.WaitGroup),
		fenceOwner:          fenceOwnerToken(),
		spawner:             NewSpawner(reg, ts, es, logger, loader, outputBase),
		jit:                 jit,
		taskStore:           ts,
		expStore:            es,
		reflection:          NewReflectionEngine(es, ts, reg, jit, logger),
		logger:              logger,
		assets:              NewAssetManager(outputBase, 0, reg), // Use outputBase for assets
		registry:            reg,
		outputBase:          outputBase,
		tmpBase:             tmpBase,
		SignalField:         sf,
		sieve:               sieve.NewSieveFromConfig(reg.System),
		budgetGuard:         NewBudgetGuard(0), // Default safety margin
		budgetSentinel:      NewBudgetSentinel(),
		pathLocks:           NewPathLockManager(),
		drafts:              NewDraftStore(),
		hashCache:           make(map[string]string),
		maxHashCacheEntries: defaultMaxHashCacheEntries,
		UseSwarm:            os.Getenv("ACAIS_MODE") == "1",
		notifyChan:          make(chan struct{}),
		decSem:              make(chan struct{}, 5), // audit NEW-P1: bound concurrent decision-resolution goroutines
		staleTaskSnapshots:  make(map[string]*staleTaskState),
		recentDecisions:     make(map[string]map[string]time.Time),
	}
	s.spawner.SignalField = sf

	s.walker = NewDAGGraphWalker(s.graphs, &s.Mu, outputBase, logger, reg)

	// Initialise lifecycle context for background services (cancelled by Stop).
	s.lifecycleCtx, s.lifecycleCancel = context.WithCancel(context.Background())
	if s.UseSwarm {
		log.Printf("WARN: swarm mode requested but not available in open core")
	}

	// Start Asset Sentry (D-GC-1 fix: pass TaskRegistry, not ITaskStore)
	// Passed as a getter, not a value: the ledger is wired by main*.go *after*
	// this constructor returns, so a value captured here would be nil for the
	// life of the engine (the first tick is an hour away — the bug would never
	// show in a test run). The getter also reads under the registry mutex,
	// which is what removes the 2026-11-02 data race.
	s.goBackground(func() { s.assets.Sentry(s.lifecycleCtx, s.taskRegistry) }) // audit L5: use goBackground so Stop() drains

	// Start terminal-graph evictor to prevent unbounded map growth (audit C4).
	// Graphs that reached a terminal state are removed from s.graphs,
	// s.cancelFuncs, and s.doneChans after a TTL.
	s.goBackground(func() { s.evictTerminalGraphs(s.lifecycleCtx) }) // audit L5: use goBackground so Stop() drains

	// Anti-Cliff: wire AssetManager + Ref-based handoff threshold into the
	// spawner so large upstream contexts are side-loaded into artifact
	// files instead of inlined into the subagent's prompt.
	s.spawner.SetAssetManager(s.assets)
	s.spawner.SetRefBasedHandoffThreshold(int(reg.System.RefBasedHandoffThreshold))

	s.loadGraphs()
	s.startStaleTaskMonitor()
	bootmetrics.Mark("scheduler")
	return s
}

// getOrCreateStepWG returns the per-task WaitGroup that tracks in-flight step
// goroutines. Lazily initialized so we don't need to touch every graph-creation
// site. (L-7.3)
func (s *DirectedEngine) getOrCreateStepWG(taskID string) *sync.WaitGroup {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.stepWGs == nil {
		s.stepWGs = make(map[string]*sync.WaitGroup)
	}
	wg, ok := s.stepWGs[taskID]
	if !ok {
		wg = &sync.WaitGroup{}
		s.stepWGs[taskID] = wg
	}
	return wg
}

// waitStepDrain blocks until all in-flight step goroutines for the given task
// have exited, or the timeout expires. This prevents late persistGraph calls
// from re-creating zombie tasks after Clear has deleted the registry entry.
// (L-7.3)
func (s *DirectedEngine) waitStepDrain(taskID string, timeout time.Duration) {
	s.Mu.RLock()
	wg := s.stepWGs[taskID]
	s.Mu.RUnlock()
	if wg == nil {
		return
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	// audit P-8-sibling: use NewTimer+Stop to avoid time.After timer leak.
	// The helper goroutine drains when wg reaches 0 (steps exit on ctx cancel);
	// a hung provider would leak both the step and helper — fix at provider level.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// WireCapabilityRouting connects the per-model per-capability telemetry store
// and pluggable model registry to the Spawner. When called with non-nil args,
// the Spawner records execution outcomes, uses Pareto-aware routing, applies
// IRT theta-based turn budgeting, and falls back to the registry for context
// window metadata. Safe to call with nil args — each is wired independently.
func (s *DirectedEngine) WireCapabilityRouting(cps *store.CapabilityProfileStore, mr *registry.ModelRegistry) {
	if cps != nil {
		s.spawner.SetCapabilityProfileStore(cps)
	}
	if mr != nil {
		s.spawner.SetModelRegistry(mr)
	}
}

// WireHooks connects config-driven hooks to DAG lifecycle events via EventBus.
// Subscribes to EventStepCompleted (step_post), EventTaskCompleted/Failed
// (task_completion), and EventDecisionRequired (decision_required). Each
// subscription runs via goBackground for clean shutdown. Safe to call with
// no hooks configured — the HookRunner will have nothing to run.
func (s *DirectedEngine) WireHooks(hooks []config.HookConfig, runtimes config.ExternalRuntimes) {
	if len(hooks) == 0 {
		return
	}
	s.hookRunner = NewHookRunner(hooks, runtimes, s.logger)

	// step_post: advisory hook after step completion
	if len(s.hookRunner.HooksForPoint(HookStepPost)) > 0 {
		obs.DefaultBus.Subscribe(string(obs.EventStepCompleted), func(ev obs.AgentEvent) error {
			s.goBackground(func() {
				s.hookRunner.Run(s.lifecycleCtx, HookStepPost, HookContext{
					Point:  HookStepPost,
					TaskID: ev.TaskID,
					StepID: ev.StepID,
					Input:  ev.Payload,
				})
			})
			return nil
		})
	}

	// task_completion: advisory hook after task reaches terminal state
	if len(s.hookRunner.HooksForPoint(HookTaskCompletion)) > 0 {
		for _, evt := range []obs.EventType{obs.EventTaskCompleted, obs.EventTaskFailed} {
			obs.DefaultBus.Subscribe(string(evt), func(ev obs.AgentEvent) error {
				s.goBackground(func() {
					s.hookRunner.Run(s.lifecycleCtx, HookTaskCompletion, HookContext{
						Point:  HookTaskCompletion,
						TaskID: ev.TaskID,
						Input:  ev.Payload,
					})
				})
				return nil
			})
		}
	}

	// decision_required: suggest auto-resolution for decisions
	if len(s.hookRunner.HooksForPoint(HookDecisionRequired)) > 0 {
		obs.DefaultBus.Subscribe(string(obs.EventDecisionRequired), func(ev obs.AgentEvent) error {
			s.goBackground(func() {
				s.hookRunner.Run(s.lifecycleCtx, HookDecisionRequired, HookContext{
					Point:  HookDecisionRequired,
					TaskID: ev.TaskID,
					StepID: ev.StepID,
					Input:  ev.Payload,
				})
			})
			return nil
		})
	}
}

// Stop shuts down background monitors and agents.
func (s *DirectedEngine) Stop() {
	s.Mu.Lock()
	if s.lifecycleCancel != nil {
		s.lifecycleCancel()
	}
	s.Mu.Unlock()

	// Wait for background goroutines (embedding, folding, reflection) to
	// finish. They use lifecycleCtx which was just cancelled, so they'll
	// exit promptly. Must be outside s.Mu to avoid deadlock.
	s.bgWg.Wait()

	// audit M7: wait for reflection's inner goroutines (hybrid merge,
	// crystallization) which are spawned by ReflectOnTask but not tracked
	// by bgWg. They derive from lifecycleCtx so they'll exit promptly.
	if s.reflection != nil {
		s.reflection.Wait()
	}
}

// SetMemoryBankStore wires the MemoryBankStore into the ReflectionEngine
// for automatic user profile extraction, and into the Spawner for
// UserProfile injection into sub-agent prompts.
// Called after construction. ADDED spawner wiring 2026-10-17.
// See docs/plan/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 3.
func (s *DirectedEngine) SetMemoryBankStore(mb *store.MemoryBankStore) {
	if s.reflection != nil {
		s.reflection.SetMemoryBankStore(mb)
	}
	if s.spawner != nil {
		s.spawner.SetMemoryBankStore(mb)
	}
}

// SetCharter sets the TaskCharter on a submitted task's graph.
// Called after SubmitWithSessionIR returns a task ID. Thread-safe.
// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P1.
func (s *DirectedEngine) SetCharter(taskID string, charter *schemas.TaskCharter) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if graph, ok := s.graphs[taskID]; ok && graph != nil {
		graph.Charter = charter
	}
}

// SetSystemOneProvider injects the System One decision model provider and
// config. Nil-safe: when provider is nil or BaseURL is empty, all integration
// points use their existing fallback logic (zero behavior change).
func (s *DirectedEngine) SetSystemOneProvider(p *providers.SystemOneProvider, cfg config.SystemOneConfig) {
	s.systemOneProvider = p
	s.systemOneCfg = cfg
	if s.spawner != nil {
		s.spawner.systemOneProvider = p
		if s.spawner.toolRouter != nil && cfg.EnableToolRouter {
			s.spawner.toolRouter.SetSystemOneProvider(p, cfg.Model)
		}
	}
	if s.sieve != nil && cfg.EnableSieve {
		s.sieve.SetSystemOneProvider(p, cfg)
	}
}

// SetArchive injects the ContextArchive into the engine and propagates it to
// the spawner for Path D (Regular Step Warm layer Archive query).
// Nil-safe: when nil, Archive queries are skipped (zero behavior change).
// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.4.
func (s *DirectedEngine) SetArchive(a *ContextArchive) {
	s.Archive = a
	if s.spawner != nil {
		s.spawner.SetArchive(a)
		s.spawner.SetProjectResolver(s.ProjectOf)
	}
}

// SetFoldEngine injects the FoldEngine for segment-based incremental fold.
// Propagates to the spawner and PromptAssembler.
// Nil-safe: when nil, fold is skipped (zero behavior change).
// See docs/plan/SEGMENT_BASED_INCREMENTAL_FOLD_DESIGN.md.
func (s *DirectedEngine) SetFoldEngine(fe *FoldEngine) {
	if s.spawner != nil {
		s.spawner.SetFoldEngine(fe)
	}
}

// goBackground launches a tracked background goroutine tied to the engine's
// lifecycle. Stop() will wait for all such goroutines to drain.
func (s *DirectedEngine) goBackground(fn func()) {
	s.bgWg.Add(1)
	go func() {
		defer s.bgWg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC in background goroutine: %v\n%s", r, debug.Stack())
			}
		}()
		fn()
	}()
}

// startGraphExecutor launches the appropriate executor goroutine (run or
// swarmWatchdog) for a task graph, based on the UseSwarm flag. All sites
// that transition a graph to GraphRunning and need to (re)start execution
// MUST call this helper instead of inlining the branch — this prevents the
// fix-propagation gap where a new site forgets the UseSwarm branch.
// (audit L-N9: third recurrence of this bug class; L-7.1 → L-N5/N6/N1/N4 → L-N9)
func (s *DirectedEngine) startGraphExecutor(ctx context.Context, taskID string) {
	// Q2-2 (register O33): the executor runs under its own child context so that
	// losing the fence can cancel this executor rather than only muting its writes
	// — run() returns on ctx.Done() and executeStep/Spawner.Spawn take this same
	// ctx, so the loser stops spending tokens and stops firing external MCP calls.
	//
	// No claim is written here. Stamping happens in persistGraph, with the manifest
	// the claim protects; a token already on disk is not evidence of a live rival
	// (crash resume would break), and a background write from this goroutine races
	// whoever owns the directory — see stampFence.
	runCtx, cancel := context.WithCancel(ctx)
	s.fenceCancels.Store(taskID, cancel)
	executor := s.swarmWatchdog
	if !s.UseSwarm {
		executor = s.run
	}
	s.goBackground(func() {
		defer cancel()
		executor(runCtx, taskID)
	})
}


func (s *DirectedEngine) GetSpawner() *Spawner {
	return s.spawner
}

func (s *DirectedEngine) GetStepResult(taskID, stepID string) *store.StepResult {
	res, _ := s.taskStore.Get(s.lifecycleCtx, taskID, stepID)
	return res
}

// GetTaskGraph returns the in-memory task graph for the given task ID, or nil
// if the task is unknown or has been evicted. Callers must treat the returned
// graph as read-only and must NOT iterate graph.Steps or other maps without
// holding s.Mu. For safe read access under the lock, use WithTaskGraph instead.
func (s *DirectedEngine) GetTaskGraph(taskID string) *schemas.TaskGraph {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.graphs[taskID]
}

// WithTaskGraph executes fn while holding RLock on the graph for taskID.
// If the task is unknown or evicted, fn is not called and the method returns false.
// Callers must treat the graph as read-only inside fn and must not retain
// references to the graph or its sub-maps after fn returns.
func (s *DirectedEngine) WithTaskGraph(taskID string, fn func(graph *schemas.TaskGraph)) bool {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	g, ok := s.graphs[taskID]
	if !ok {
		return false
	}
	fn(g)
	return true
}

func (s *DirectedEngine) TaskKnown(taskID string) bool {
	s.Mu.RLock()
	_, ok := s.graphs[taskID]
	s.Mu.RUnlock()
	return ok
}

// ProjectOf returns the project space identifier for the given task.
// If the task is unknown or has no project set, it returns "default".
// This enables search callers to resolve projectScope from a taskID
// (Project Space & Memory Tiering design Phase 3).
func (s *DirectedEngine) ProjectOf(taskID string) string {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return projectOfGraph(s.graphs[taskID])
}

// projectOfGraph is the lock-free form of ProjectOf, for callers that already
// hold a *TaskGraph and therefore need no lookup at all.
//
// It exists because ProjectOf takes s.Mu.RLock, and sync.RWMutex is not
// reentrant: a goroutine that already holds s.Mu.Lock and then calls ProjectOf
// blocks forever waiting on a read lock it can never get — no error, no
// timeout, no cancellation. tryBacktrack (core/cycle_breaker.go) ran exactly
// that way after 40617ac0f wired ProjectOf into buildPreviousSummary, which
// tryBacktrack calls under its own write lock: every cycle-breaker backtrack
// deadlocked the task.
//
// The rule this encodes is worth keeping: a helper that needs one field of a
// struct the caller already holds should take the field, not re-acquire the
// lock to look it up.
func projectOfGraph(graph *schemas.TaskGraph) string {
	if graph == nil || graph.ProjectID == "" {
		return "default"
	}
	return graph.ProjectID
}

// ─── Clear ────────────────────────────────────────────────────────────────

func (s *DirectedEngine) Clear(taskID string) error {
	s.Mu.Lock()
	cancel := s.cancelFuncs[taskID]
	delete(s.graphs, taskID)
	delete(s.cancelFuncs, taskID)
	delete(s.doneChans, taskID)
	s.Mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// L-7.3: wait for in-flight step goroutines to drain before clearing the
	// task store, so late persistGraph calls don't re-create a zombie task.
	s.waitStepDrain(taskID, 5*time.Second)

	s.Mu.Lock()
	delete(s.stepWGs, taskID)
	s.Mu.Unlock()
	s.dropFence(taskID)

	// audit HIGH (2026-10-28): clear EffectTracker — Clear is the catch-all
	// terminal path, so ensure cleanup even if finalize/cancel was missed.
	if s.spawner != nil {
		s.spawner.ClearEffectTracker(taskID)
	}
	// audit MEDIUM (2026-10-28): clear lastPersistAt to prevent unbounded
	// memory growth — sync.Map had Store but no Delete.
	s.lastPersistAt.Delete(taskID)

	if _, err := s.taskStore.ClearTask(s.lifecycleCtx, taskID); err != nil { // audit L-3.2: surface ClearTask error
		return fmt.Errorf("clear task store for %s: %w", taskID, err)
	}

	tmpDir := filepath.Join(s.tmpBase, taskID)
	_ = os.RemoveAll(tmpDir)
	return nil
}

// evictTerminalGraphs periodically removes task graphs that have reached a
// terminal state (completed/failed/cancelled) from the in-memory maps after
// a TTL. This prevents unbounded growth of s.graphs, s.cancelFuncs, and
// s.doneChans in long-running servers (audit finding C4).
// The task data itself remains in the persistent TaskRegistry for later queries.
func (s *DirectedEngine) evictTerminalGraphs(ctx context.Context) {
	const evictTTL = 30 * time.Minute
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := time.Now()
		var toEvict []string
		s.Mu.RLock()
		for taskID, graph := range s.graphs {
			if !isTerminalStatus(graph.Status) {
				continue
			}
			if graph.CompletedAt == nil {
				ts := now
				graph.CompletedAt = &ts
				continue
			}
			if now.Sub(*graph.CompletedAt) >= evictTTL {
				toEvict = append(toEvict, taskID)
			}
		}
		s.Mu.RUnlock()

		if len(toEvict) == 0 {
			continue
		}

		s.Mu.Lock()
		for _, taskID := range toEvict {
			if _, ok := s.graphs[taskID]; !ok {
				continue
			}
			delete(s.graphs, taskID)
			delete(s.cancelFuncs, taskID)
			delete(s.doneChans, taskID)
			delete(s.stepWGs, taskID)
			s.dropFence(taskID)
			s.cleanRecentDecisions(taskID)
		}
		s.Mu.Unlock()
	}
}

// isTerminalStatus reports whether a GraphStatus is terminal (no further
// transitions expected).
func isTerminalStatus(status schemas.GraphStatus) bool {
	switch status {
	case schemas.GraphCompleted, schemas.GraphCompletedWithSkips,
		schemas.GraphFailed, schemas.GraphCancelled, schemas.GraphOrphaned:
		return true
	}
	return false
}

// cleanRecentDecisions removes all recentDecisions entries for the given taskID.
// Called from evictTerminalGraphs to prevent unbounded growth of recentDecisions.
func (s *DirectedEngine) cleanRecentDecisions(taskID string) {
	s.recentDecisionsMu.Lock()
	delete(s.recentDecisions, taskID)
	s.recentDecisionsMu.Unlock()
}
