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
	"time"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ─── Graph persistence & loading ──────────────────────────────────────────

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
				// P10: load pinned baseline BEFORE acquiring s.Mu so the
				// network I/O (GetBaseline + LoadPinnedRole) doesn't block
				// all graph operations (audit P-6). Safe because graph isn't
				// in s.graphs yet — no concurrent access to it.
				if graph.Status == schemas.GraphRunning {
					s.applyPinnedBaseline(&graph)
				}
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

					// (applyPinnedBaseline already called above, before Lock)

					ctx, cancel := context.WithCancel(s.lifecycleCtx)
					s.cancelFuncs[graph.TaskID] = cancel
					// Ensure done channel exists
					if _, ok := s.doneChans[graph.TaskID]; !ok {
						s.doneChans[graph.TaskID] = make(chan struct{})
					}
					// audit H3/L-N4: startGraphExecutor handles UseSwarm branching.
					s.startGraphExecutor(ctx, graph.TaskID)
				}
				s.Mu.Unlock()
			}
		}
	}
}

// applyPinnedBaseline loads the pinned Role version from the archive and
// injects it into graph.SessionRoles so the spawner uses the pinned version
// instead of the live registry. Nil-safe: skips when BaselineStore is nil
// or no baseline row exists. Old tasks (no baseline) fall back gracefully.
func (s *DirectedEngine) applyPinnedBaseline(graph *schemas.TaskGraph) {
	if s.BaselineStore == nil {
		return
	}
	ctx := s.lifecycleCtx
	baseline, err := s.BaselineStore.GetBaseline(ctx, graph.TaskID)
	if err != nil || baseline == nil {
		return
	}
	if baseline.RoleID != "" && baseline.RoleVersion != "" {
		role, err := s.BaselineStore.LoadPinnedRole(ctx, baseline.RoleID, baseline.RoleVersion)
		if err != nil {
			s.logger.Log("EventBaselineLoadError", graph.TaskID, "", map[string]any{
				"role_id": baseline.RoleID, "role_version": baseline.RoleVersion,
				"error": err.Error(),
			})
			return
		}
		if graph.SessionRoles == nil {
			graph.SessionRoles = make(map[string]any)
		}
		graph.SessionRoles[role.ID] = role
		s.logger.Log("EventBaselineApplied", graph.TaskID, "", map[string]any{
			"role_id": role.ID, "role_version": role.Version,
		})
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
	// Q2-2 (register O33): compare-before-write. A graph this process no longer
	// holds means another engine adopted the same outputs/<taskID>, so manifest,
	// artifacts and the ledger row all belong to that engine's view of reality.
	// Refusing is not enough on its own — recordFenceLoss cancels the executor,
	// which is what makes this a stop instead of a mute.
	if !s.holdsFence(graph.TaskID) {
		s.recordFenceLoss(graph.TaskID)
		return
	}

	// H-5 (2026-10-04): Debounce — skip if the same graph was persisted
	// < 200ms ago. Eliminates redundant persists at adjacent state
	// transitions (e.g., scheduler_dag.go:147+162 are < 10ms apart).
	// 200ms is conservative: step transitions involve LLM calls (> 1s),
	// so meaningful persists are never skipped.
	now := time.Now()
	if last, ok := s.lastPersistAt.Load(graph.TaskID); ok {
		if t, ok := last.(time.Time); ok {
			if now.Sub(t) < 200*time.Millisecond {
				return
			}
		}
	}
	s.lastPersistAt.Store(graph.TaskID, now)

	// P-2.1 Step 1: marshal under RLock (read-only) instead of Lock (write).
	// This allows concurrent readers to proceed during the ~1-3ms marshal,
	// eliminating ~95% of the global mutex contention. The only write is
	// graph.Artifacts = artifacts, which gets a brief separate Lock section.
	// Note: the manifest will contain the PREVIOUS artifacts (marshal happens
	// before the artifacts write); artifacts.json has the NEW artifacts.
	s.Mu.RLock()
	graph.LockDecisionHistory() // P-2.1 Step 2: prevent Spawner from writing DecisionHistory during marshal
	outDir := filepath.Join(s.outputBase, graph.TaskID)
	artifacts := s.buildArtifactsLocked(graph)
	manifestData, mErr := json.Marshal(graph) // P-1.8: compact marshal on hot path
	if mErr != nil {
		log.Printf("WARN: persistGraph: failed to marshal manifest for task %s: %v", graph.TaskID, mErr)
	}
	var artifactsData []byte
	if len(artifacts) > 0 {
		var aErr error
		artifactsData, aErr = json.Marshal(artifacts)
		if aErr != nil {
			log.Printf("WARN: persistGraph: failed to marshal artifacts for task %s: %v", graph.TaskID, aErr)
		}
	}
	var statusData []byte
	var statusStr string
	if s.taskRegistry() != nil {
		var sErr error
		statusData, sErr = json.Marshal(graph.ToStatusDict())
		if sErr != nil {
			log.Printf("WARN: persistGraph: failed to marshal task state for %s: %v", graph.TaskID, sErr)
		}
		statusStr = string(graph.Status)
	}
	graph.UnlockDecisionHistory()
	s.Mu.RUnlock()

	// Brief write lock for the artifacts field update only (<1µs).
	s.Mu.Lock()
	graph.Artifacts = artifacts
	s.Mu.Unlock()

	// File I/O outside lock (P-H5)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		log.Printf("WARN: persistGraph: failed to create output dir %s: %v", outDir, err)
	}
	if len(manifestData) > 0 {
		if werr := os.WriteFile(filepath.Join(outDir, "manifest.json"), manifestData, 0644); werr != nil {
			log.Printf("WARN: persistGraph: failed to write manifest.json for task %s: %v", graph.TaskID, werr)
		} else {
			// Q2-2: this is the stamp point — the claim is published with the state
			// it protects, so a second engine driving the same directory finds a
			// token to contend with (see stampFence for why not earlier).
			s.stampFence(graph.TaskID)
		}
	}
	if len(artifactsData) > 0 {
		if werr := os.WriteFile(filepath.Join(outDir, "artifacts.json"), artifactsData, 0644); werr != nil {
			log.Printf("WARN: persistGraph: failed to write artifacts.json for task %s: %v", graph.TaskID, werr)
		}
	}
	if tr := s.taskRegistry(); tr != nil && len(statusData) > 0 {
		if serr := tr.SaveTask(s.lifecycleCtx, graph.TaskID, statusStr, statusData); serr != nil {
			log.Printf("WARN: persistTaskState: failed to save task state for %s: %v", graph.TaskID, serr)
		}
	}
}

// buildArtifactsLocked builds the artifact contract list from graph.OutputFiles.
// Caller MUST hold s.Mu.Lock() (reads graph fields). Extracted from
// persistGraphLocked to allow marshal-under-lock + write-outside-lock (P-H5).
func (s *DirectedEngine) buildArtifactsLocked(graph *schemas.TaskGraph) []schemas.ArtifactContract {
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
		// SHA-256 of the on-disk artifact (cached to avoid re-hashing).
		s.hashCacheMu.Lock()
		if s.hashCache == nil {
			s.hashCache = make(map[string]string)
		}
		hash, cached := s.hashCache[of.Path]
		s.hashCacheMu.Unlock()
		if cached {
			ac.SHA256 = hash
		} else if sum, hashErr := sha256File(of.Path); hashErr == nil {
			ac.SHA256 = sum
			s.recordHashCacheEntry(of.Path, sum)
		} else {
			// audit L1 (2026-10-29): a failed read produces a hash of
			// only the bytes read before the error. Do NOT set or cache
			// it — a partial hash would be silently reused for all future
			// lookups, poisoning artifact provenance. Leave ac.SHA256 empty.
			log.Printf("WARN: persistGraph: failed to hash artifact %s: %v", of.Path, hashErr)
		}
		// Provenance: source steps that fed this step
		if owner, ok := graph.Steps[of.StepID]; ok {
			constraints := owner.OutputContract.Clone()
			ac.RequiredFields = constraints.RequiredFields
			ac.OptionalFields = constraints.OptionalFields
			ac.TypeSchema = constraints.TypeSchema
			ac.SourceRefs = append(ac.SourceRefs, constraints.SourceRefs...)
			for _, dep := range owner.DependsOn {
				if depStep, ok := graph.Steps[dep]; ok && depStep.ResultRef != "" {
					ac.SourceRefs = append(ac.SourceRefs, depStep.ResultRef)
				}
			}
		}
		artifacts = append(artifacts, ac)
	}
	return artifacts
}

// sha256File returns the hex SHA-256 of the file at path. It closes the handle
// before returning, so callers looping over many artifacts do not accumulate
// open handles (a defer inside the loop would hold every handle until the
// enclosing function returns).
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
