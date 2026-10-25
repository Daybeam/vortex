package core

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/search"
	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_helpers.go — Utility and helper functions for the decision engine.
// Extracted from scheduler_decision.go on 2026-09-15.
// Contains: retryWait, swarmWatchdog, updateStepEmbedding, updateNodeEmbedding,
// foldNode, archiveStep.

func (s *DirectedEngine) retryWait(role *config.Role, attempt int) int {
	if role != nil {
		s.registry.Mu.RLock()
		defer s.registry.Mu.RUnlock()
		for _, mcpID := range role.BoundMCPIDs() {
			if mcp := s.registry.MCPs[mcpID]; mcp != nil && mcp.RateLimit != nil {
				waits := mcp.RateLimit.RetryWaitSeconds
				if len(waits) > 0 {
					idx := attempt
					if idx >= len(waits) {
						idx = len(waits) - 1
					}
					return waits[idx]
				}
			}
		}
	}
	defaults := []int{5, 30, 120}
	if attempt >= len(defaults) {
		return defaults[len(defaults)-1]
	}
	return defaults[attempt]
}

func (s *DirectedEngine) swarmWatchdog(ctx context.Context, taskID string) {
	delay := s.registry.System.SwarmFallbackDelay
	if delay <= 0 {
		delay = 10
	}

	// audit P-NEW-3: use NewTimer + Stop instead of time.After to avoid timer leak.
	timer := time.NewTimer(time.Duration(delay) * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return
	}

	// audit L-1.5: hold RLock for graph.Status check + graph.Steps iteration
	// to prevent TOCTOU and concurrent map iteration race with MutateGraphTopology.
	s.Mu.RLock()
	graph := s.graphs[taskID]
	if graph == nil || graph.Status != schemas.GraphRunning {
		s.Mu.RUnlock()
		return
	}

	// Check for activity: if no steps have started running or completed, take over.
	hasActivity := false
	for _, step := range graph.Steps {
		if step.Status == schemas.StepRunning || step.Status == schemas.StepOK || step.Status == schemas.StepPartial {
			hasActivity = true
			break
		}
	}
	s.Mu.RUnlock()

	if !hasActivity {
		s.logger.Log(EventSwarmWatchdogTriggered, taskID, "", map[string]any{
			"delay_seconds": delay,
			"message":       "No swarm activity detected, taking over locally",
		})
		s.goBackground(func() { s.run(ctx, taskID) }) // audit M8: use goBackground so Stop() drains
	}
}

func (s *DirectedEngine) updateStepEmbedding(taskID, stepID string, text string) {
	s.Mu.RLock()
	graph := s.graphs[taskID]
	s.Mu.RUnlock()
	if graph == nil {
		return
	}

	hub := NewContextHub(s.registry, graph, s.expStore)
	emb, modelID, err := hub.EmbedWithFallback(s.lifecycleCtx, text)
	if err != nil {
		s.logger.Log("EventSemanticDegraded", taskID, stepID, map[string]any{
			"error": err.Error(),
		})
		return
	}

	s.Mu.Lock()
	if step, ok := graph.Steps[stepID]; ok {
		step.Embedding = emb
		step.EmbeddingModel = modelID
	}
	s.Mu.Unlock()
}

func (s *DirectedEngine) updateNodeEmbedding(taskID, nodeID string, text string) {
	s.Mu.RLock()
	graph := s.graphs[taskID]
	s.Mu.RUnlock()
	if graph == nil {
		return
	}

	hub := NewContextHub(s.registry, graph, s.expStore)
	emb, modelID, err := hub.EmbedWithFallback(s.lifecycleCtx, text)
	if err != nil {
		s.logger.Log("EventSemanticDegraded", taskID, "", map[string]any{
			"node_id": nodeID,
			"error":   err.Error(),
		})
		return
	}

	s.Mu.Lock()
	if node, ok := graph.ContextTree[nodeID]; ok {
		node.Embedding = emb
		node.EmbeddingModel = modelID
	}
	s.Mu.Unlock()
}

func (s *DirectedEngine) foldNode(taskID, nodeID string) {
	// audit L-1.6: hold RLock for graph.ContextTree lookup + graph.Steps reads
	// to prevent concurrent map access race with MutateGraphTopology.
	s.Mu.RLock()
	graph := s.graphs[taskID]
	if graph == nil {
		s.Mu.RUnlock()
		return
	}

	node := graph.ContextTree[nodeID]
	if node == nil {
		s.Mu.RUnlock()
		return
	}

	s.logger.Log("EventNodeFoldingStarted", taskID, "", map[string]any{"node_id": nodeID})

	// Collect all step tasks and results for this node
	// audit P-H4: batch-fetch all step results in one query instead of N.
	stepResults, _ := s.taskStore.GetBatch(s.lifecycleCtx, taskID, node.StepIDs)
	var content strings.Builder
	for _, sid := range node.StepIDs {
		step := graph.Steps[sid]
		if step == nil {
			continue
		}
		fmt.Fprintf(&content, "Step %s: %s\n", sid, step.Task)
		if res := stepResults[sid]; res != nil {
			fmt.Fprintf(&content, "Result: %v\n", res.Data)
		}
	}
	s.Mu.RUnlock()

	// ACAIS: Folding must preserve a checksum for integrity
	h := sha256.New()
	h.Write([]byte(content.String()))
	checksum := fmt.Sprintf("sha256:%x", h.Sum(nil))

	// Use a lightweight LLM call to summarize
	summaryPrompt := fmt.Sprintf("Summarize the following task execution history into a concise conclusion (max 100 words):\n\n%s", content.String())

	// We use the spawner with a generic auditor role
	res, err := s.spawner.Spawn(s.lifecycleCtx, &SpawnRequest{
		TaskID:      taskID,
		StepID:      "fold_" + nodeID,
		RoleID:      "auditor",
		Task:        summaryPrompt,
		RoutingMode: schemas.RoutingModeLegacy,
		Hub:         NewContextHub(s.registry, graph, s.expStore),
	})

	s.Mu.Lock()
	node.Checksum = checksum // audit L-1.6: write checksum under Lock
	if err == nil {
		node.Summary = fmt.Sprintf("%v", res.Output.Result)
		node.Status = schemas.NodeResolved
		s.logger.Log("EventNodeFolded", taskID, "", map[string]any{"node_id": nodeID, "summary": node.Summary})
	} else {
		node.Status = schemas.NodeAbandoned
		s.logger.Log("EventNodeFoldingFailed", taskID, "", map[string]any{"node_id": nodeID, "error": err.Error()})
	}
	s.Mu.Unlock()
	s.persistGraph(graph) // audit PERF-HIGH-1: I/O outside lock (was persistGraphLocked)
}

// archiveStep ingests a MemoryItem into the ContextArchive after step completion.
// Called from handleOutput when s.Archive != nil. Single-task scope only —
// the MemoryItem is tagged with graph.TaskID so Search can filter by task.
func (s *DirectedEngine) archiveStep(graph *schemas.TaskGraph, step *schemas.Step, output schemas.SubagentOutput) {

	summary, _ := output.Result["summary"].(string)
	if summary == "" {
		if raw, ok := output.Result["output"].(string); ok && len(raw) > 200 {
			summary = raw[:200] + "..."
		} else if raw, ok := output.Result["output"].(string); ok {
			summary = raw
		}
	}

	item := MemoryItem{
		TaskID:    graph.TaskID,
		NodeID:    step.ID,
		Intent:    step.Task,
		Summary:   summary,
		StepIDs:   []string{step.ID},
		Timestamp: time.Now(),
	}

	if err := s.Archive.Append(item); err != nil {
		s.logger.Log("EventArchiveError", graph.TaskID, step.ID, map[string]any{"err": err.Error()})
	}
}

// ─── Intent-Shift Branching (B11, WIRED 2026-10-25) ──────────────────────
// See docs/plan/CONTEXT_TREE_INTENT_SHIFT_BRANCHING_DESIGN.md.

// defaultIntentShiftThreshold is the cosine-similarity floor below which a
// step counts as an intent shift from the current context node. Same order
// as SegmentManager's segmentSealThreshold: deliberately conservative, so
// only a real drift forks the tree (avoids fragmentation on noisy embeddings).
const defaultIntentShiftThreshold = 0.20

// Telemetry modes for EventIntentShiftBranch.
const (
	intentShiftModeSemantic  = "semantic"
	intentShiftModeHeuristic = "heuristic"
)

// intentShiftSettings resolves the semantic branching policy from the
// registry. Defaults (nil pointer / non-positive threshold) preserve the
// pre-2026-10-25 behavior: enabled at defaultIntentShiftThreshold.
// Registry reads are lock-free by convention (feature flags are hot-reloadable;
// same pattern as prompt_assembler.go's MemoryBankEnabled read).
func (s *DirectedEngine) intentShiftSettings() (enabled bool, threshold float64) {
	enabled, threshold = true, defaultIntentShiftThreshold
	if s.registry == nil {
		return enabled, threshold
	}
	if p := s.registry.System.IntentShiftBranchingEnabled; p != nil {
		enabled = *p
	}
	if t := s.registry.System.IntentShiftThreshold; t > 0 {
		threshold = t
	}
	return enabled, threshold
}

// intentShifted compares two CACHED embeddings and reports whether the step
// has drifted below threshold (B11 semantic pull). Pure function — no engine
// state, no I/O: the decision must stay far under B11's 50ms budget, so
// embedding computation is never attempted here. A cold cache (either side
// empty) or a dimension mismatch returns drifted=false with score -1, sending
// the caller to the heuristic fallback; the async backfills
// (updateStepEmbedding in handleStepSuccess, updateNodeEmbedding at node
// creation) warm the cache for the next transition.
// NOTE: pkg/search.CosineSimilarity returns 0.0 for unequal lengths, which is
// indistinguishable from maximal drift — hence the explicit length-equality
// guard instead of trusting a 0.0 score as "orthogonal".
func intentShifted(nodeEmb, stepEmb []float32, threshold float64) (drifted bool, driftScore float64) {
	if len(nodeEmb) == 0 || len(stepEmb) == 0 || len(nodeEmb) != len(stepEmb) {
		return false, -1
	}
	score := search.CosineSimilarity(nodeEmb, stepEmb)
	return score < threshold, score
}

// transitionContextTree implements Phase 3.5: Context Tree Transition.
// When a step succeeds with an intent shift — semantic (cached-embedding
// cosine below IntentShiftThreshold, WIRED 2026-10-25) or heuristic
// ("new task" in the model's Assumptions, pre-2026-10-25 fallback) — a child
// context node is created and CurrentNodeID is updated. Extracted from
// executeStep so tests can verify branching without reimplementing the logic.
func (s *DirectedEngine) transitionContextTree(graph *schemas.TaskGraph, step *schemas.Step, result *SpawnResult) {
	s.Mu.Lock()
	var branchTelemetry map[string]any // logged after unlock (I/O outside lock)
	currNode := graph.ContextTree[graph.CurrentNodeID]
	if currNode != nil {
		currNode.StepIDs = append(currNode.StepIDs, step.ID)

		// Detect Branch Point
		if step.Status == schemas.StepOK {
			enabled, threshold := s.intentShiftSettings()
			needsNewNode := false
			branchMode := ""
			driftScore := -1.0 // telemetry: -1 = not a semantic branch

			// Heuristic fallback: the model self-reports a new task in its
			// Assumptions. Retained because the semantic path needs warm
			// cached vectors (cold cache on the first transition after a
			// branch — see intentShifted).
			if reason := result.Output.Assumptions; len(reason) > 0 &&
				strings.Contains(strings.Join(reason, " "), "new task") {
				needsNewNode = true
				branchMode = intentShiftModeHeuristic
			}

			// Semantic Check (Semantic Pull, B11): cosine(currNode, step)
			// < threshold ⇒ IntentShift ⇒ TreeBranching. Cached vectors
			// only — never blocks on an embedding call (plan §3.3).
			if !needsNewNode && enabled {
				if drift, score := intentShifted(currNode.Embedding, step.Embedding, threshold); drift {
					needsNewNode = true
					branchMode = intentShiftModeSemantic
					driftScore = score
				}
			}

			if needsNewNode {
				newNodeID := fmt.Sprintf("node_%s", uuid.New().String()[:6])
				// Intent carries the drifted task text (plan §3.4) so the
				// node's async embedding reflects the NEW intent — that
				// embedding is what the next transition's drift check
				// compares against. Fallback keeps the old provenance
				// string when the step has no task text.
				intent := step.Task
				if intent == "" {
					intent = "Adaptive Transition from " + step.ID
				}
				newNode := &schemas.ContextNode{
					ID:       newNodeID,
					ParentID: currNode.ID,
					Intent:   intent,
					Status:   schemas.NodeActive,
					Metadata: make(map[string]any),
				}

				// Inherit root metadata (Behavior Contract requirement)
				for k, v := range currNode.Metadata {
					newNode.Metadata[k] = v
				}
				// Note: LocalSymbolIndex is intentionally left empty/nil to isolate interference.

				graph.ContextTree[newNodeID] = newNode
				graph.CurrentNodeID = newNodeID

				branchTelemetry = map[string]any{
					"mode":        branchMode,
					"drift_score": driftScore,
					"threshold":   threshold,
					"parent_node": currNode.ID,
					"new_node_id": newNodeID,
				}

				// Async Embedding of new intent
				s.goBackground(func() { s.updateNodeEmbedding(graph.TaskID, newNodeID, newNode.Intent) })

				// Async Folding of previous node
				s.goBackground(func() { s.foldNode(graph.TaskID, currNode.ID) })
			}
		}
	}
	s.Mu.Unlock()

	// Telemetry outside the lock: logger.Log fans out to the event bus, and
	// subscribers must never be able to re-enter engine locks.
	if branchTelemetry != nil {
		s.logger.Log(EventIntentShiftBranch, graph.TaskID, step.ID, branchTelemetry)
	}
}
