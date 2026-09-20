package core

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

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

	s.Mu.RLock()
	graph := s.graphs[taskID]
	s.Mu.RUnlock()

	if graph == nil || graph.Status != schemas.GraphRunning {
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
	s.Mu.RLock()
	graph := s.graphs[taskID]
	s.Mu.RUnlock()
	if graph == nil {
		return
	}

	node := graph.ContextTree[nodeID]
	if node == nil {
		return
	}

	s.logger.Log("EventNodeFoldingStarted", taskID, "", map[string]any{"node_id": nodeID})

	// Collect all step tasks and results for this node
	var content strings.Builder
	for _, sid := range node.StepIDs {
		step := graph.Steps[sid]
		if step == nil {
			continue
		}
		fmt.Fprintf(&content, "Step %s: %s\n", sid, step.Task)
		if res, err := s.taskStore.Get(s.lifecycleCtx, taskID, sid); err == nil {
			fmt.Fprintf(&content, "Result: %v\n", res.Data)
		}
	}

	// ACAIS: Folding must preserve a checksum for integrity
	h := sha256.New()
	h.Write([]byte(content.String()))
	node.Checksum = fmt.Sprintf("sha256:%x", h.Sum(nil))

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
	if err == nil {
		node.Summary = fmt.Sprintf("%v", res.Output.Result)
		node.Status = schemas.NodeResolved
		s.logger.Log("EventNodeFolded", taskID, "", map[string]any{"node_id": nodeID, "summary": node.Summary})
	} else {
		node.Status = schemas.NodeAbandoned
		s.logger.Log("EventNodeFoldingFailed", taskID, "", map[string]any{"node_id": nodeID, "error": err.Error()})
	}
	s.persistGraph(graph)
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
