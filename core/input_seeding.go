package core

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/daybeam/vortex/schemas"
)

// seedTaskInputs copies declared input directories into the task workspace
// before the DAG starts executing. For each step with a non-empty InputDir,
// the directory {workspaceRoot}/{inputDir} is recursively copied to
// {outputBase}/{taskID}/workspace/{inputDir}.
//
// When workspaceRoot is empty or inputDir is empty for all steps, this is
// a no-op (backward compatible).
//
// See docs/completed/2026-09-19/WORKSPACE_AND_SANDBOX_REDESIGN.md §2.2 ③.
func (s *DirectedEngine) seedTaskInputs(graph *schemas.TaskGraph, outputBase string) error {
	if graph.WorkspaceRoot == "" {
		return nil
	}

	hasInputDir := false
	for _, step := range graph.Steps {
		if step.InputDir != "" {
			hasInputDir = true
			break
		}
	}
	if !hasInputDir {
		return nil
	}

	taskWorkspace := filepath.Join(outputBase, graph.TaskID, "workspace")
	if err := os.MkdirAll(taskWorkspace, 0755); err != nil {
		return fmt.Errorf("create task workspace: %w", err)
	}

	for _, step := range graph.Steps {
		if step.InputDir == "" {
			continue
		}
		src := filepath.Join(graph.WorkspaceRoot, step.InputDir)
		dst := filepath.Join(taskWorkspace, step.InputDir)

		info, err := os.Stat(src)
		if err != nil {
			s.logger.Log("EventInputSeedSkip", graph.TaskID, step.ID, map[string]any{
				"input_dir": step.InputDir,
				"reason":    err.Error(),
			})
			continue
		}

		if info.IsDir() {
			if err := copyDir(src, dst); err != nil {
				return fmt.Errorf("seed input dir %q: %w", step.InputDir, err)
			}
		} else {
			data, err := os.ReadFile(src)
			if err != nil {
				return fmt.Errorf("seed input file %q: %w", step.InputDir, err)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, data, 0644); err != nil {
				return fmt.Errorf("seed input file %q: %w", step.InputDir, err)
			}
		}

		s.logger.Log("EventInputSeed", graph.TaskID, step.ID, map[string]any{
			"input_dir": step.InputDir,
			"src":       src,
			"dst":       dst,
		})
	}
	return nil
}
