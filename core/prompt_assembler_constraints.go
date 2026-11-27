package core

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// buildConstraintsBlock renders the protected cache marker that closes the
// stable prefix. Template parse/execute failures retain their original error
// contract and abort Build before any volatile or archive blocks are appended.
func (s *PromptAssembler) buildConstraintsBlock(
	role *config.Role,
	capability string,
	providerCfg *config.ProviderConfig,
	toolConstraints []string,
	mergedContext map[string]any,
	taskID string,
) (schemas.ContentBlock, error) {
	var constraintsPart strings.Builder
	if role != nil && role.Rules != "" {
		constraintsPart.WriteString("## Hard Rules & Constraints\n")
		constraintsPart.WriteString(role.Rules)
		constraintsPart.WriteString("\n\n")
	}

	// A role may opt out of the framework's own framing; see Role.MinimalPrompt.
	minimalPrompt := role != nil && role.MinimalPrompt

	if tc := schemas.ToolConstraintFragment(toolConstraints); tc != "" {
		constraintsPart.WriteString(tc)
	} else {
		// General tool usage instruction when not restricted
		constraintsPart.WriteString("\n## Tool Usage\nYou have access to multiple tools. You MUST use them whenever they can help you fulfill the task or verify information. Do not just describe what you would do; actually call the tools.")
	}
	if !minimalPrompt {
		// Both fragments below assert things about the role's surface and its
		// answer shape. They are right for a general executor and wrong for a
		// role that owns its own contract — see Role.MinimalPrompt.
		//
		// The surface fragment is rendered from the SAME policy the dispatcher
		// enforces (coreToolPolicyFromRole; spawner_mcp.go filters schemas with
		// it, spawner_tool_exec.go refuses calls with it). A hardcoded list was
		// wrong the moment appworld_agent set core_tools to read_file/write_file:
		// the prompt kept advertising a code sandbox the role no longer had, and
		// the model — measured 2026-10-19 on 4fab96f_3, 7 of 8 core calls —
		// answered by writing code against a sandbox with no app data instead of
		// calling the MCP tools that did. Advertisement follows capability.
		corePol := coreToolPolicyFromRole(role)
		hasWrite := corePol.allows("write_file")
		hasRead := corePol.allows("read_file")
		hasCode := corePol.allows("execute_code")
		if hasWrite || hasRead || hasCode {
			if hasWrite || hasRead {
				constraintsPart.WriteString("\n\n## Core File & Code Tools\n")
			} else {
				constraintsPart.WriteString("\n\n## Core Code Tool\n")
			}
			constraintsPart.WriteString("You have access to built-in tools:\n")
			if hasWrite {
				constraintsPart.WriteString("- **write_file**: Write text/JSON to a file under the task output directory. Use for saving reports, results, or generated content.\n")
			}
			if hasRead {
				constraintsPart.WriteString("- **read_file**: Read a file from the task output directory. Large files are auto-truncated.\n")
			}
			if hasCode {
				constraintsPart.WriteString("- **execute_code**: Run code in python/node/bun/lua for computation or data processing.\n")
			}
			if hasWrite || hasRead {
				if hasCode {
					constraintsPart.WriteString("Use write_file/read_file for ALL file operations. Use execute_code for computation. Do not search for non-existent file tools.\n")
				} else {
					// Naming the absence is load-bearing, not pedantry: without it a
					// model that wants a computation has no reason to prefer a tool
					// call over inventing a sandbox invocation, and burns its turns
					// on a refusal loop.
					constraintsPart.WriteString("Use write_file/read_file for ALL file operations. Do not search for non-existent file tools.\n")
				}
			}
			// The output directory is per-TASK, not per-step: spawner_tool_exec.go
			// resolves it as filepath.Join(outputBase, req.TaskID), and every step
			// of a DAG shares that TaskID. So it is the default hand-off channel
			// between steps — the one that needs no planner annotation, no
			// InputMapping, and no workspace key. Saying so is load-bearing: a step
			// that does not know a predecessor wrote there will re-derive the same
			// data with the same tools, which is both wasted turns and (measured
			// 2026-10-19, 4fab96f_3) a run that hits MAX_TURNS_EXHAUSTED.
			switch {
			case hasRead && hasWrite:
				constraintsPart.WriteString("This output directory is SHARED by every step of this task. A predecessor step may have written intermediate results here — list it and read those files before re-deriving anything an earlier step already produced. When your own result is large and a later step only needs the conclusion, write the distilled version here instead of making the next step reconstruct it.\n")
			case hasRead:
				constraintsPart.WriteString("This output directory is SHARED by every step of this task. A predecessor step may have written intermediate results here — list it and read those files before re-deriving anything an earlier step already produced.\n")
			case hasWrite:
				constraintsPart.WriteString("This output directory is SHARED by every step of this task, so a later step can read what you write here. When your result is large and a later step only needs the conclusion, write the distilled version here instead of passing the whole thing through your answer.\n")
			}
			// D4 fix (DELEGATION_HANDOVER_REDESIGN.md §4.4): in delegation mode,
			// append the absolute path so the host knows WHERE the shared output
			// directory is. Without this, the prompt says "shared output
			// directory" but never gives the path — the host cannot write/read.
			if s.registry.System.DelegationMode {
				s.outputBaseMu.Lock()
				ob := s.outputBase
				s.outputBaseMu.Unlock()
				if ob != "" {
					constraintsPart.WriteString(fmt.Sprintf("The absolute path is %q; write deliverables there so later steps can read them.\n", filepath.Join(ob, taskID)))
				}
				// D4b (DELEGATION_HANDOVER_REDESIGN.md §4.4): the deliverable
				// contract. handleStepSuccess registers a step's output file ONLY
				// when the result carries "output_ref" (a path relative to the task
				// output directory). Without this line a delegated host writes the
				// file correctly but graph.output_files stays empty, so the caller
				// cannot discover the deliverable from get_task_status.
				constraintsPart.WriteString("If you produce a deliverable file, set \"output_ref\" in your final JSON result to its path relative to the task output directory (e.g. \"report.md\"), optionally alongside \"summary\" and \"format\". The orchestrator registers it as this step's output file.\n")
			}
		} else if corePol.restricted {
			// Restricted to nothing: say so once, without a heading, so the model
			// does not spend a turn looking for tools that were deliberately
			// withheld. This is the planner's shape.
			constraintsPart.WriteString("\n\nThere are no built-in file or code tools available to you in this role. Everything you know must come from the tools listed above.\n")
		}
		constraintsPart.WriteString("\n\n")
		constraintsPart.WriteString(schemas.BuildOutputConstraintWithTemplate(capability, s.registry.PromptOverrides.OutputContract))
	} else {
		constraintsPart.WriteString("\n\n")
	}

	// Template execution on the combined final part
	tmpl, err := template.New("system").Parse(constraintsPart.String())
	if err != nil {
		return schemas.ContentBlock{}, fmt.Errorf("template parse error: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, mergedContext); err != nil {
		return schemas.ContentBlock{}, fmt.Errorf("template execute error: %w", err)
	}
	return MarkProtectedWithCategory(schemas.ContentBlock{
		Text:         buf.String(),
		CacheControl: "ephemeral", // Marker 1: End of System Prompt
	}, CategoryConstraints), nil
}
