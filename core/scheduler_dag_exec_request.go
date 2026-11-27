package core

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// buildStepSpawnRequest assembles one retry's prompt and spawn inputs. Locking,
// security sanitization, batch reads, and coordination edges match the original
// inline sequence in executeStep.
func (s *DirectedEngine) buildStepSpawnRequest(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step, dynTemp *float32) (*SpawnRequest, context.Context) {
	taskID := graph.TaskID
	var treeHistory []map[string]any
	s.Mu.RLock()
	if g, ok := s.graphs[taskID]; ok {
		currID := g.CurrentNodeID
		for currID != "" {
			node := g.ContextTree[currID]
			if node == nil {
				break
			}
			treeHistory = append(treeHistory, map[string]any{
				"id": node.ID, "intent": node.Intent, "summary": node.Summary, "status": node.Status,
			})
			currID = node.ParentID
		}
		for i, j := 0, len(treeHistory)-1; i < j; i, j = i+1, j-1 {
			treeHistory[i], treeHistory[j] = treeHistory[j], treeHistory[i]
		}
	}
	s.Mu.RUnlock()

	resolvedInputs := s.resolveStepInputs(ctx, graph, step, taskID)
	s.recordContextRefEdges(ctx, graph, step, taskID)

	traceID := graph.TraceID
	if traceID == "" {
		traceID = NewTraceID()
	}
	spanID := NewSpanID()
	ctx = ContextWithTrace(ctx, traceID, spanID)

	hub := NewContextHub(s.registry, graph, s.expStore)
	var predecessorSkillID string
	for _, depID := range step.DependsOn {
		if dep, ok := graph.Steps[depID]; ok && (dep.Status == schemas.StepOK || dep.Status == schemas.StepPartial) {
			predecessorSkillID = dep.RoleID
			break
		}
	}
	taskText := s.buildStepTaskText(ctx, graph, step, taskID)

	// Per-task append-only knowledge board; failure is non-fatal.
	var sharedCtx *store.SharedContext
	scDir := filepath.Join(s.outputBase, taskID, "shared_context")
	sharedCtx, scErr := store.NewSharedContext(scDir, taskID)
	if scErr != nil {
		s.logger.Log("EventSharedContextInitFailed", taskID, step.ID, map[string]any{"error": scErr.Error()})
		sharedCtx = nil
	}
	return &SpawnRequest{
		TaskID:                   taskID,
		StepID:                   step.ID,
		TraceID:                  traceID,
		SpanID:                   spanID,
		RoleID:                   step.RoleID,
		SubmitterIsAdmin:         graph.SubmitterIsAdmin,
		Task:                     taskText,
		AdditionalSkills:         step.AdditionalSkills,
		AdditionalMCPs:           step.AdditionalMCPs,
		AdditionalToolAllowlists: step.AdditionalToolAllowlists,
		ContextRefs:              step.ContextRefs,
		Temperature:              dynTemp,
		FrequencyPenalty:         nil,
		ProviderOverride:         step.ProviderOverride,
		RoutingMode:              step.RoutingMode,
		CompressionHint:          step.CompressionHint,
		TurnsBudgetBonus:         step.TurnsBudgetBonus,
		MaxTurnsCap:              step.MaxTurnsCap,
		SessionRoot:              graph.WorkspaceRoot,
		OwnerID:                  graph.OwnerID,
		PredecessorSkillID:       predecessorSkillID,
		Metadata:                 map[string]any{"context_tree_path": treeHistory},
		Hub:                      hub,
		InputMapping:             resolvedInputs,
		Isolation:                step.Isolation,
		AdditionalPromptContext:  step.AdditionalPromptContext,
		EnableStepPlan:           step.EnableStepPlan,
		PreviousOutputSummary:    step.PreviousOutputSummary,
		SharedContext:            sharedCtx,
		ExecutionProtocol:        step.ExecutionProtocol,
		ACPConfig:                step.ACPConfig,
	}, ctx
}

func (s *DirectedEngine) recordContextRefEdges(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step, taskID string) {
	if len(step.ContextRefs) == 0 {
		return
	}
	// audit P-H2: batch-fetch all referenced step results in one query.
	refStepIDs := make([]string, 0, len(step.ContextRefs))
	for _, ref := range step.ContextRefs {
		parts := strings.SplitN(ref, ":", 2)
		if len(parts) == 2 {
			refStepIDs = append(refStepIDs, parts[0])
		}
	}
	refResults, _ := s.taskStore.GetBatch(ctx, taskID, refStepIDs)
	for _, ref := range step.ContextRefs {
		parts := strings.SplitN(ref, ":", 2)
		if len(parts) == 2 {
			if res := refResults[parts[0]]; res != nil && res.Data != nil {
				s.recordCoordinationEdge(graph, parts[0], step.ID, "reference", estimateJSONSize(res.Data))
			}
		}
	}
}

// buildStepTaskText preserves Sieve partial-output sanitization and the
// automatic upstream-result block used when no explicit InputMapping exists.
func (s *DirectedEngine) buildStepTaskText(ctx context.Context, graph *schemas.TaskGraph, step *schemas.Step, taskID string) string {
	taskText := step.Task
	for _, depID := range step.DependsOn {
		if dep, ok := graph.Steps[depID]; ok && dep.PartialOutput != "" {
			sanitized := sanitizePartialOutputForPassthrough(dep.PartialOutput, dep.LastError, graph.OwnerID)
			if sanitized != "" {
				taskText = fmt.Sprintf("%s\n\nUPSTREAM PARTIAL OUTPUT (step %s was intercepted by Sieve Guardian; verify or build on this):\n%s", taskText, depID, sanitized)
			}
			break
		}
	}
	if len(step.DependsOn) == 0 || len(step.InputMapping) > 0 {
		return taskText
	}
	upstreamIDs := make([]string, 0, len(step.DependsOn))
	for _, depID := range step.DependsOn {
		if dep, ok := graph.Steps[depID]; ok && dep.Status == schemas.StepOK {
			upstreamIDs = append(upstreamIDs, depID)
		}
	}
	if len(upstreamIDs) == 0 {
		return taskText
	}
	upResults, _ := s.taskStore.GetBatch(ctx, taskID, upstreamIDs)
	var upstreamBlock string
	for _, depID := range upstreamIDs {
		res := upResults[depID]
		if res == nil || res.Data == nil {
			continue
		}
		// Render JSON for machine consumers and bound prompt growth.
		var summary string
		if b, err := json.Marshal(res.Data); err == nil {
			summary = string(b)
		} else {
			summary = fmt.Sprintf("%v", res.Data)
		}
		if r := []rune(summary); len(r) > 1200 {
			summary = string(r[:1200]) + "...(truncated)"
		}
		artifactRef := filepath.Join("outputs", taskID, depID)
		upstreamBlock += fmt.Sprintf("\n  %s: %s\n  (full output at %s/ — use read_file if needed)", depID, summary, artifactRef)
	}
	if upstreamBlock != "" {
		taskText = fmt.Sprintf("%s\n\nUPSTREAM RESULTS (from completed predecessor steps — use these directly, do not re-fetch):%s", taskText, upstreamBlock)
	}
	return taskText
}
