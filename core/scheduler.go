package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/pkg/registry"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// DirectedEngine owns all task graphs and drives execution.
// Go advantage: each task graph runs in its own goroutine.
// No asyncio event loop — the language runtime handles concurrency.
type DirectedEngine struct {
	Mu          sync.RWMutex
	graphs      map[string]*schemas.TaskGraph
	cancelFuncs map[string]context.CancelFunc
	doneChans   map[string]chan struct{} // For long-polling

	spawner        *Spawner
	jit            *JITManager
	taskStore      store.ITaskStore
	expStore       store.IExperienceStore
	reflection     *ReflectionEngine
	logger         *Logger
	assets         *AssetManager
	registry       *config.Registry
	outputBase     string
	tmpBase        string
	SignalField    *SignalField
	sieve          *Sieve
	budgetGuard    *BudgetGuard
	budgetSentinel *BudgetSentinel
	walker         *DAGGraphWalker

	// JITSessions holds POC session-scoped JIT interpreter state (design doc
	// playbook/jit-session-poc-design-2026.md). Nil-safe: set explicitly by
	// main.go/main_web.go after NewDirectedEngine returns, rather than
	// threaded through the constructor, to avoid breaking the 4 existing
	// test call sites that construct a DirectedEngine directly. executeStep
	// checks for nil before use.
	JITSessions *JITSessionManager

	// TaskRegistry persists task-level lifecycle state so GetStatus() can answer
	// for graphs that are absent from the in-process map — which happens under the
	// Master/Proxy split and after a restart, where the submitting process and the
	// querying process are not the same one.
	// Nil-safe: set explicitly by main*.go after NewDirectedEngine returns, same
	// rationale as JITSessions above. All call sites must nil-check before use.
	TaskRegistry store.TaskRegistry

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

	UseSwarm    bool
	agents      []interfaces.AutonomousProvider
	convMonitor interfaces.ConvergenceMonitor
	notifyChan  chan struct{}
	notifyMu    sync.Mutex

	// lifecycleCtx is cancelled by Stop() to shut down background services
	// (swarm agents, convergence monitor, parameter adaptor) and async poll
	// goroutines. Replaces context.Background() at service-start sites.
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
	jit *JITManager,
	logger *Logger,
	sf *SignalField,
	outputBase, tmpBase string,
	loader *ResourceLoader,
) *DirectedEngine {
	s := &DirectedEngine{
		graphs:              make(map[string]*schemas.TaskGraph),
		cancelFuncs:         make(map[string]context.CancelFunc),
		doneChans:           make(map[string]chan struct{}),
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
		sieve:               newSieveFromConfig(reg.System),
		budgetGuard:         NewBudgetGuard(0), // Default safety margin
		budgetSentinel:      NewBudgetSentinel(),
		pathLocks:           NewPathLockManager(),
		hashCache:           make(map[string]string),
		maxHashCacheEntries: defaultMaxHashCacheEntries,
		UseSwarm:            os.Getenv("ACAIS_MODE") == "1",
		notifyChan:          make(chan struct{}),
	}
	s.spawner.SignalField = sf

	s.walker = NewDAGGraphWalker(s.graphs, &s.Mu, outputBase, logger, reg)

	// Initialise lifecycle context for background services (cancelled by Stop).
	s.lifecycleCtx, s.lifecycleCancel = context.WithCancel(context.Background())
	if s.UseSwarm {
		// Swarm mode (ACAIS) is not included in this build. Log a warning
		// and continue in single-agent mode.
		s.logger.Log("EventSwarmUnavailable", "", "", map[string]any{
			"message": "ACAIS_MODE=1 but swarm components are not available in the open core; running in single-agent mode",
		})
	}

	// Start Asset Sentry
	s.goBackground(func() { s.assets.Sentry(s.lifecycleCtx, ts) }) // audit L5: use goBackground so Stop() drains

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
	return s
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

// Stop shuts down background monitors and agents.

// Stop shuts down background monitors and agents.
func (s *DirectedEngine) Stop() {
	s.Mu.Lock()
	if s.lifecycleCancel != nil {
		s.lifecycleCancel()
	}
	for _, a := range s.agents {
		a.Stop()
	}
	if s.convMonitor != nil {
		s.convMonitor.Stop()
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

// goBackground launches a tracked background goroutine tied to the engine's
// lifecycle. Stop() will wait for all such goroutines to drain.
func (s *DirectedEngine) goBackground(fn func()) {
	s.bgWg.Add(1)
	go func() {
		defer s.bgWg.Done()
		fn()
	}()
}

func (s *DirectedEngine) Refresh() error {
	s.loadGraphs()
	return nil
}

func (s *DirectedEngine) loadGraphs() {
	files, err := os.ReadDir(s.outputBase)
	if err != nil {
		return
	}
	for _, f := range files {
		if f.IsDir() {
			manifestPath := filepath.Join(s.outputBase, f.Name(), "manifest.json")
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				continue
			}
			var graph schemas.TaskGraph
			if err := json.Unmarshal(data, &graph); err == nil {
				s.Mu.Lock()
				// Check if already in memory to avoid duplicate goroutines and pointer replacement
				if _, ok := s.graphs[graph.TaskID]; ok {
					// If already managed, don't re-start run loop
					s.Mu.Unlock()
					continue
				}

				s.graphs[graph.TaskID] = &graph
				// Resume running tasks
				if graph.Status == schemas.GraphRunning {
					// Reset zombie steps to pending so s.run() picks them up
					for _, step := range graph.Steps {
						if step.Status == schemas.StepRunning {
							step.Status = schemas.StepPending
						}
					}

					ctx, cancel := context.WithCancel(s.lifecycleCtx)
					s.cancelFuncs[graph.TaskID] = cancel
					// Ensure done channel exists
					if _, ok := s.doneChans[graph.TaskID]; !ok {
						s.doneChans[graph.TaskID] = make(chan struct{})
					}
					if !s.UseSwarm {
						// audit H3: use goBackground so Stop()'s bgWg.Wait()
						// actually waits for resumed tasks to drain.
						s.goBackground(func() { s.run(ctx, graph.TaskID) })
					}
				}
				s.Mu.Unlock()
			}
		}
	}
}

// detectReadDeps scans a step's tool-call trace for file reads and matches
// them to upstream steps' declared output files, building coordination edges
// for the read-dependency graph.
//
// FIX (2026-08-19, playbook addendum): this function's original signature
// (graph, step *schemas.Step, result *SpawnResult) assumed step.Trace and
// step.OutputFiles fields that do not exist on schemas.Step -- trace data
// lives in store.StepResult.Trace (see doSpawn's taskStoreSet call), and
// output files live in the flat graph.OutputFiles slice (each entry already
// tagged with the producing StepID), not per-step. The function's only call
// site (inside SubmitWithSessionIR, at task-submission time before any step
// had executed) referenced undefined step/result variables and didn't even
// make semantic sense there. Removed that call site and corrected this
// function's signature/body to take the trace directly, since callers are in
// the best position to supply it (e.g. from a completed step's
// store.StepResult or the local `trace` slice already built inside doSpawn).
// WIRED IN (2026-08-21): called from executeStep after handleOutput on the
// success path, using the step's trace as persisted to the task store by
// doSpawn (spawner.go). Wiring this in also surfaced and fixed an
// unrelated, pre-existing bug in executeStep: the success path used to
// call handleOutput and return immediately, before ever reaching the Sieve
// Guardian check / Context Tree transition / exit-criteria refine logic
// that lives further down in the same function -- see the FIX comment at
// executeStep's `if err == nil` block for details.
func (s *DirectedEngine) detectReadDeps(graph *schemas.TaskGraph, stepID string, trace []store.ToolInteraction, snap *SnapshotIndex) []schemas.ReadDep {
	return s.walker.DetectReadDeps(graph, stepID, trace, snap)
}

func (s *DirectedEngine) persistGraph(graph *schemas.TaskGraph) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.persistGraphLocked(graph)
}

// persistGraphLocked does the actual persist work. Caller MUST hold s.Mu.Lock().
// Use persistGraph (without "Locked") when the caller does not hold the lock.
func (s *DirectedEngine) persistGraphLocked(graph *schemas.TaskGraph) {
	outDir := filepath.Join(s.outputBase, graph.TaskID)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		log.Printf("WARN: persistGraph: failed to create output dir %s: %v", outDir, err)
	}

	// Artifact delivery contract (ADDED 2026-08-17)
	// For each primary output file that exists on disk, compute SHA-256 and
	// provenance (upstream step refs), then write artifacts.json alongside
	// manifest.json. Downstream consumers read structured facts (status,
	// sources, integrity) instead of guessing from natural-language text.
	artifacts := make([]schemas.ArtifactContract, 0, len(graph.OutputFiles))
	for _, of := range graph.OutputFiles {
		if !of.IsPrimary {
			continue
		}
		ac := schemas.ArtifactContract{
			Path:        of.Path,
			Format:      of.Format,
			SizeBytes:   of.SizeBytes,
			Status:      "draft",
			GeneratedAt: of.CreatedAt,
			OwnerStep:   of.StepID,
		}
		// SHA-256 of the on-disk artifact (cached to avoid re-hashing on
		// every persistGraph call — output files are write-once).
		s.hashCacheMu.Lock()
		if s.hashCache == nil {
			s.hashCache = make(map[string]string)
		}
		hash, cached := s.hashCache[of.Path]
		s.hashCacheMu.Unlock()
		if cached {
			ac.SHA256 = hash
		} else if f, err := os.Open(of.Path); err == nil {
			h := sha256.New()
			if _, err := io.Copy(h, f); err != nil {
				log.Printf("WARN: persistGraph: failed to hash artifact %s: %v", of.Path, err)
			}
			f.Close()
			ac.SHA256 = fmt.Sprintf("%x", h.Sum(nil))
			s.hashCacheMu.Lock()
			// Evict entire cache if it has grown too large (unbounded growth
			// protection for long-running servers). Safe because the cache is
			// only an optimization — cleared entries will be re-hashed on next access.
			if len(s.hashCache) >= s.maxHashCacheEntries {
				s.hashCache = make(map[string]string)
			}
			s.hashCache[of.Path] = ac.SHA256
			s.hashCacheMu.Unlock()
		}
		// Provenance: source steps that fed this step
		if owner, ok := graph.Steps[of.StepID]; ok {
			for _, dep := range owner.DependsOn {
				if depStep, ok := graph.Steps[dep]; ok && depStep.ResultRef != "" {
					ac.SourceRefs = append(ac.SourceRefs, depStep.ResultRef)
				}
			}
		}
		artifacts = append(artifacts, ac)
	}

	// Marshal the graph. Safe because caller holds s.Mu.Lock() — no concurrent
	// step goroutine can write to graph fields while we read them.
	graph.Artifacts = artifacts
	data, err := json.MarshalIndent(graph, "", "  ")
	if err == nil && !graph.IsSmartRouted {
		if werr := os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0644); werr != nil {
			log.Printf("WARN: persistGraph: failed to write manifest.json for task %s: %v", graph.TaskID, werr)
		}
	}
	// Write artifacts.json as a separate lightweight contract file
	if len(artifacts) > 0 {
		artData, aerr := json.MarshalIndent(artifacts, "", "  ")
		if aerr == nil {
			if werr := os.WriteFile(filepath.Join(outDir, "artifacts.json"), artData, 0644); werr != nil {
				log.Printf("WARN: persistGraph: failed to write artifacts.json for task %s: %v", graph.TaskID, werr)
			}
		}
	}

	// Mirror task-level lifecycle state into the registry. Doing it here covers
	// every state transition that already checkpoints via persistGraph, so status
	// stays queryable from processes that do not own the in-memory graph.
	if s.TaskRegistry != nil {
		if data, err := json.Marshal(graph.ToStatusDict()); err == nil {
			if serr := s.TaskRegistry.SaveTask(s.lifecycleCtx, graph.TaskID, string(graph.Status), data); serr != nil {
				log.Printf("WARN: persistTaskState: failed to save task state for %s: %v", graph.TaskID, serr)
			}
		}
	}
}

func (s *DirectedEngine) GetSpawner() *Spawner {
	return s.spawner
}

func (s *DirectedEngine) GetStepResult(taskID, stepID string) *store.StepResult {
	res, _ := s.taskStore.Get(s.lifecycleCtx, taskID, stepID)
	return res
}

func (s *DirectedEngine) TaskKnown(taskID string) bool {
	s.Mu.RLock()
	_, ok := s.graphs[taskID]
	s.Mu.RUnlock()
	return ok
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
	_, _ = s.taskStore.ClearTask(s.lifecycleCtx, taskID)

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
		s.Mu.Lock()
		for taskID, graph := range s.graphs {
			if !isTerminalStatus(graph.Status) {
				continue
			}
			// If CompletedAt was not set at the transition site, stamp it
			// on first observation so we can age it out next cycle.
			if graph.CompletedAt == nil {
				ts := now
				graph.CompletedAt = &ts
				continue
			}
			if now.Sub(*graph.CompletedAt) >= evictTTL {
				delete(s.graphs, taskID)
				delete(s.cancelFuncs, taskID)
				delete(s.doneChans, taskID)
			}
		}
		s.Mu.Unlock()
	}
}

// isTerminalStatus reports whether a GraphStatus is terminal (no further
// transitions expected).
func isTerminalStatus(status schemas.GraphStatus) bool {
	switch status {
	case schemas.GraphCompleted, schemas.GraphCompletedWithSkips,
		schemas.GraphFailed, schemas.GraphCancelled:
		return true
	}
	return false
}

func (s *DirectedEngine) Broadcast() {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	close(s.notifyChan)
	s.notifyChan = make(chan struct{})
}

func (s *DirectedEngine) GetNotifyChan() chan struct{} {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notifyChan
}

func (s *DirectedEngine) CancelTask(taskID string) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	cancel, ok := s.cancelFuncs[taskID]
	graph, ok2 := s.graphs[taskID]
	if !ok || !ok2 {
		return fmt.Errorf("task %q not found or already terminal", taskID)
	}
	cancel()
	graph.Status = schemas.GraphCancelled
	s.persistGraphLocked(graph) // audit H7: persist cancelled state to prevent zombie tasks on restart
	return nil
}

func (s *DirectedEngine) PatchWorkspace(taskID string, data map[string]any) error {
	s.Mu.Lock()
	graph, ok := s.graphs[taskID]
	if !ok {
		s.Mu.Unlock()
		return fmt.Errorf("task %q not found", taskID)
	}

	if graph.GlobalWorkspace == nil {
		graph.GlobalWorkspace = make(map[string]any)
	}

	// Merge patch data
	for k, v := range data {
		graph.GlobalWorkspace[k] = v
	}
	s.Mu.Unlock()

	// Audit Log
	s.logger.Log("EventWorkspacePatched", taskID, "", map[string]any{
		"patched_keys": func() []string {
			keys := make([]string, 0, len(data))
			for k := range data {
				keys = append(keys, k)
			}
			return keys
		}(),
		"reason": "manual_intervention",
	})

	return nil
}

func (s *DirectedEngine) recordCoordinationEdge(graph *schemas.TaskGraph, source, target, edgeType string, payload int64) {
	s.walker.RecordCoordinationEdge(graph, source, target, edgeType, payload)
}

// GetCrossFamilyVerifier returns a provider ID from a different model family
// than the given provider (identified by provider ID). Falls back to "" when
// the provider is unknown or no cross-family provider is available.
func (e *DirectedEngine) GetCrossFamilyVerifier(providerID string) string {
	return e.walker.GetCrossFamilyVerifier(providerID)
}
