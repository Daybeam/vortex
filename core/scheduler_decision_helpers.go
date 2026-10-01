package core

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_helpers.go — Utility and helper functions for the decision engine.
// Extracted from scheduler_decision.go on 2026-09-15.
// Contains: retryWait, swarmWatchdog, updateStepEmbedding, updateNodeEmbedding,
// foldNode, archiveStep.

func (s *DirectedEngine) retryWait(role *config.Role, attempt int) int {
	if role != nil {
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

	select {
	case <-time.After(time.Duration(delay) * time.Second):
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
	defer s.Mu.Unlock()
	node.Checksum = checksum // audit L-1.6: write checksum under Lock
	if err == nil {
		node.Summary = fmt.Sprintf("%v", res.Output.Result)
		node.Status = schemas.NodeResolved
		s.logger.Log("EventNodeFolded", taskID, "", map[string]any{"node_id": nodeID, "summary": node.Summary})
	} else {
		node.Status = schemas.NodeAbandoned
		s.logger.Log("EventNodeFoldingFailed", taskID, "", map[string]any{"node_id": nodeID, "error": err.Error()})
	}
	s.persistGraphLocked(graph)
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

// transitionContextTree implements Phase 3.5: Context Tree Transition.
// When a step succeeds with "new task" in its assumptions, a child context
// node is created and CurrentNodeID is updated. Extracted from executeStep
// so tests can verify branching without reimplementing the logic.
func (s *DirectedEngine) transitionContextTree(graph *schemas.TaskGraph, step *schemas.Step, result *SpawnResult) {
	s.Mu.Lock()
	currNode := graph.ContextTree[graph.CurrentNodeID]
	if currNode != nil {
		currNode.StepIDs = append(currNode.StepIDs, step.ID)

		// Detect Branch Point
		if step.Status == schemas.StepOK {
			needsNewNode := false
			reason := result.Output.Assumptions
			if len(reason) > 0 && strings.Contains(strings.Join(reason, " "), "new task") {
				needsNewNode = true
			}

			// Semantic Check (Semantic Pull)
			if !needsNewNode && len(currNode.Embedding) > 0 {
				// In a real execution, we would embed the result here
				// For now, we rely on the heuristic or trigger async embed
			}

			if needsNewNode {
				newNodeID := fmt.Sprintf("node_%s", uuid.New().String()[:6])
				newNode := &schemas.ContextNode{
					ID:       newNodeID,
					ParentID: currNode.ID,
					Intent:   "Adaptive Transition from " + step.ID,
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

				// Async Embedding of new intent
				s.goBackground(func() { s.updateNodeEmbedding(graph.TaskID, newNodeID, newNode.Intent) })

				// Async Folding of previous node
				s.goBackground(func() { s.foldNode(graph.TaskID, currNode.ID) })
			}
		}
	}
	s.Mu.Unlock()
}
