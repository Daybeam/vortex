package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/types"
	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_success.go — Phase 5: Step success handling.
// Extracted from handleOutput per GOD_CLASS_REFLECTION_EXECUTION_PLAN.md Step 3.

func (s *DirectedEngine) handleStepSuccess(graph *schemas.TaskGraph, step *schemas.Step, result *SpawnResult) {
	taskID := graph.TaskID
	output := result.Output
	conf := output.Confidence

	s.Mu.Lock()
	step.Status = schemas.StepOK
	step.LastError = ""
	step.ResultRef = result.Ref
	s.Mu.Unlock()
	s.logger.LogWithConfidence(obs.EventStepCompleted, taskID, step.ID, conf, nil)
	payload := map[string]any{"confidence": conf}
	if output.Usage != nil {
		payload["usage"] = map[string]any{
			"prompt_tokens":      output.Usage.PromptTokens,
			"completion_tokens":  output.Usage.CompletionTokens,
			"total_tokens":       output.Usage.TotalTokens,
			"cache_write_tokens": output.Usage.CacheWriteTokens,
			"cache_read_tokens":  output.Usage.CacheReadTokens,
		}
	}
	payload["role_id"] = step.RoleID
	s.publishEvent(taskID, step.ID, obs.EventStepCompleted, payload)

	if output.Usage != nil {
		s.costTracker.Record(result.ModelID, taskID, step.ID, output.Usage.PromptTokens, output.Usage.CompletionTokens, output.Usage.CacheWriteTokens, output.Usage.CacheReadTokens)
	}

	if step.TriggerError != "" && step.AdditionalPromptContext != "" {
		s.DepositExperienceEvolution(s.lifecycleCtx, graph, step)
	}

	s.goBackground(func() { s.updateStepEmbedding(graph.TaskID, step.ID, step.Task) })

	if step.MergeStrategy != "" && step.TargetKey != "" {
		s.Mu.Lock()
		if graph.GlobalWorkspace == nil {
			graph.GlobalWorkspace = make(map[string]any)
		}

		val := output.Result
		if specific, ok := output.Result[step.TargetKey]; ok {
			val = map[string]any{step.TargetKey: specific}
		}

		switch step.MergeStrategy {
		case "append":
			existing, _ := graph.GlobalWorkspace[step.TargetKey].([]any)
			graph.GlobalWorkspace[step.TargetKey] = append(existing, val)
		case "unique":
			existing, _ := graph.GlobalWorkspace[step.TargetKey].([]any)
			found := false
			strVal := fmt.Sprintf("%v", val)
			for _, v := range existing {
				if fmt.Sprintf("%v", v) == strVal {
					found = true
					break
				}
			}
			if !found {
				graph.GlobalWorkspace[step.TargetKey] = append(existing, val)
			}
		case "overlay":
			graph.GlobalWorkspace[step.TargetKey] = val
		}
		s.Mu.Unlock()

		payloadSize := int64(len(fmt.Sprintf("%v", val)))
		s.recordCoordinationEdge(graph, step.ID, "global_workspace", "shared_vfs", payloadSize)

		s.logger.Log("EventProgrammaticMerge", graph.TaskID, step.ID, map[string]any{
			"strategy": step.MergeStrategy,
			"key":      step.TargetKey,
		})
	}

	s.Mu.Lock()
	AutoDepositResult(graph, step.ID, output.Result)
	s.Mu.Unlock()
	s.logger.Log("EventAutoDeposit", graph.TaskID, step.ID, nil)

	if s.Archive != nil {
		s.archiveStep(graph, step, output)
	}

	if s.SignalField != nil {
		// audit RACE-2: read BaseIntensity under RLock — Deposit() takes the
		// write lock internally and is not reentrant, so we snapshot the value
		// first rather than holding the lock across the Deposit call.
		s.SignalField.Mu.RLock()
		base := s.SignalField.BaseIntensity
		s.SignalField.Mu.RUnlock()
		intensity := base * conf
		s.SignalField.Deposit(types.LayerCritical, step.ID, intensity)

		// audit L-1.2: collect dependent step IDs under RLock to prevent
		// concurrent map iteration race with MutateGraphTopology.
		s.Mu.RLock()
		dependents := make([]string, 0, len(graph.Steps))
		for _, other := range graph.Steps {
			for _, dep := range other.DependsOn {
				if dep == step.ID && other.Status == schemas.StepPending {
					dependents = append(dependents, other.ID)
				}
			}
		}
		s.Mu.RUnlock()
		for _, depID := range dependents {
			s.SignalField.Deposit(types.LayerDependency, depID, intensity*0.5)
		}
	}

	if ref, ok := output.Result["output_ref"].(string); ok && ref != "" {
		summary, _ := output.Result["summary"].(string)
		format, _ := output.Result["format"].(string)
		sizeBytes, _ := output.Result["size_bytes"].(float64)
		s.Mu.Lock()
		graph.OutputFiles = append(graph.OutputFiles, schemas.OutputFile{
			StepID:    step.ID,
			Path:      ref,
			Format:    format,
			Summary:   summary,
			SizeBytes: int(sizeBytes),
			IsPrimary: true,
			Deliver:   step.Deliver,
			CreatedAt: time.Now(),
		})
		s.Mu.Unlock()
	}

	s.registerContractOutput(graph, step, taskID)

	resultJSON, _ := json.Marshal(output.Result)
	assetFile, err := s.assets.Handle(graph.TaskID, step.ID, resultJSON, "json", "working")
	if err == nil && assetFile != nil {
		assetFile.Summary = "Side-loaded large JSON result"
		s.Mu.Lock()
		graph.OutputFiles = append(graph.OutputFiles, *assetFile)
		step.ResultRef = assetFile.Path
		s.Mu.Unlock()
	}
}

// registerContractOutput appends the step's declared deliverable to
// graph.OutputFiles when it can be resolved on disk.
//
// Three guards, one per failure this field has already produced or would
// produce:
//
//   - A relative path with an empty WorkspaceRoot would be resolved against the
//     daemon's own working directory, so a file that merely shares a name with
//     the contract would be registered as this task's deliverable. Registering
//     the wrong file is worse than registering none (→176).
//   - The step may already be registered by the `output_ref` branch above. That
//     branch only fires when the model volunteers the field; when it does, both
//     branches see the same (step, path) and the run would report two
//     deliverables. `len(graph.OutputFiles)` is read as the deliverable count
//     (scheduler_decision_finalize.go) and walked (dag_walker.go), so a
//     duplicate is a wrong number, not a harmless repeat.
//   - A missing file is logged, never fatal: registration is an audit field,
//     not a judgement (→172).
func (s *DirectedEngine) registerContractOutput(graph *schemas.TaskGraph, step *schemas.Step, taskID string) {
	contractPath := step.OutputContract.Path
	if contractPath == "" {
		return
	}
	fullPath := contractPath
	if !filepath.IsAbs(fullPath) {
		if graph.WorkspaceRoot == "" {
			s.logger.Log(obs.EventContractFileMissing, taskID, step.ID, map[string]any{
				"contract_path": contractPath,
				"reason":        "no_workspace_root",
			})
			return
		}
		fullPath = filepath.Join(graph.WorkspaceRoot, contractPath)
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		s.logger.Log(obs.EventContractFileMissing, taskID, step.ID, map[string]any{
			"contract_path":  contractPath,
			"workspace_root": graph.WorkspaceRoot,
			"reason":         "stat_failed",
			"error":          err.Error(),
		})
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	for _, existing := range graph.OutputFiles {
		if existing.StepID == step.ID && existing.Path == contractPath {
			return
		}
	}
	graph.OutputFiles = append(graph.OutputFiles, schemas.OutputFile{
		StepID:    step.ID,
		Path:      contractPath,
		Format:    step.OutputContract.Format,
		Summary:   "Registered from output_contract",
		SizeBytes: int(info.Size()),
		IsPrimary: true,
		Deliver:   step.Deliver,
		CreatedAt: info.ModTime(),
	})
}
