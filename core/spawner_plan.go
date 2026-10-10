package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// PLAN_MODE_RULES is the system prompt prefix injected during the Planner
// Phase. It instructs the model to produce a markdown plan without executing.
const PLAN_MODE_RULES = `You are in PLAN MODE. Your job is to analyze the task below and produce a clear, step-by-step execution plan in markdown.

Rules:
1. Do NOT execute the task. Only plan it.
2. Break the task into ordered sub-actions (use numbered list or checkboxes).
3. For each sub-action, specify: what to do, which file/tool, and expected outcome.
4. If you see potential risks or edge cases, note them.
5. Keep the plan concise — one line per sub-action when possible.

Output your plan as markdown. The plan will be given to the executor phase.`

// runPlannerPhase executes the Planner Phase: a single provider call with
// PLAN_MODE_RULES that produces a markdown plan. The plan is written to
// <workspace>/step_plans/<step_id>.md and returned as text.
//
// This is a simplified implementation (no MCP tools during planning — the
// model plans from the task description alone). See STEP_PLAN_MODE_DESIGN.md
// §2.2 for the full design.
func (s *Spawner) runPlannerPhase(ctx context.Context, req *SpawnRequest) (string, error) {
	if req.Hub == nil {
		return "", fmt.Errorf("planner phase: Hub is nil")
	}

	role := req.Hub.GetRole(req.RoleID)
	if role == nil {
		return "", fmt.Errorf("planner phase: role %q not found", req.RoleID)
	}

	// Resolve provider — use the first candidate config.
	candidates := s.buildCandidateConfigs(ctx, req, role)
	if len(candidates) == 0 {
		return "", fmt.Errorf("planner phase: no provider candidates")
	}
	cand := candidates[0]
	pCfg := s.resolveCandidateConfig(req.Hub, role, cand.providerID, cand.model)

	provider, err := providers.Get(pCfg, req.Hub.Registry.ExternalRuntimes)
	if err != nil {
		return "", fmt.Errorf("planner phase: get provider: %w", err)
	}

	// Build planning request — no MCP servers (no tool execution).
	system := PLAN_MODE_RULES
	if role.Instruction != "" {
		system += "\n\n## Role Context\n" + role.Instruction
	}
	if req.AdditionalPromptContext != "" {
		system += "\n\n## Additional Context\n" + req.AdditionalPromptContext
	}

	// P1-6 (2026-10-10): Path C — inject upstream step summaries from Archive.
	// Gives the planner visibility into what previous steps in the same task
	// have already produced, enabling better step-level planning.
	// See docs/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.3.
	if s.archive != nil && req.TaskID != "" {
		upstreamSummaries := s.archive.SearchRecentStepSummaries(req.TaskID, 3)
		if len(upstreamSummaries) > 0 {
			system += "\n\n## Upstream Step Summaries\n"
			system += "The following summaries are from previously completed steps in this task. Use them to understand what has already been done and plan accordingly:\n\n"
			for _, sm := range upstreamSummaries {
				stepLabel := sm.NodeID
				if stepLabel == "" && len(sm.StepIDs) > 0 {
					stepLabel = sm.StepIDs[0]
				}
				if stepLabel == "" {
					stepLabel = "unknown"
				}
				system += fmt.Sprintf("- **Step %s**: %s\n", stepLabel, sm.Summary)
			}
		}
	}

	planCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := provider.Complete(planCtx, providers.CompleteRequest{
		System:    system,
		User:      req.Task,
		Model:     cand.model,
		MaxTokens: 2048,
	})
	if err != nil {
		return "", fmt.Errorf("planner phase: provider call: %w", err)
	}

	// Plan Gate: validate plan is non-empty.
	plan := resp.Text
	if len(plan) < 10 {
		return "", fmt.Errorf("planner phase: produced empty or trivial plan (len=%d)", len(plan))
	}

	// Persist plan to file for observability.
	planDir := filepath.Join(s.outputBase, "step_plans")
	_ = os.MkdirAll(planDir, 0755)
	planPath := filepath.Join(planDir, req.StepID+".md")
	_ = os.WriteFile(planPath, []byte(plan), 0644)

	return plan, nil
}

// shouldEnableStepPlan decides whether to activate step-plan mode for a
// request that doesn't have EnableStepPlan explicitly set. It uses
// DetermineComplexity (keyword heuristic) and optionally a System One
// probe for the fuzzy zone.
//
// If req.EnableStepPlan is already true, returns true immediately.
// If the probe is nil or fails, falls back to keyword matching.
func (s *Spawner) shouldEnableStepPlan(req *SpawnRequest, probe providers.Provider) bool {
	if req.EnableStepPlan {
		return true
	}

	// Phase A: keyword heuristic (zero cost).
	if schemas.DetermineComplexity(req.Task, 0) {
		return true
	}

	// Short tasks that didn't hit keywords → likely simple.
	if len(req.Task) < 100 {
		return false
	}

	// Phase B: System One probe for the fuzzy zone.
	if probe == nil {
		return false
	}
	complex, err := ProbeTaskComplexity(probe, req.Task)
	if err != nil {
		return false // probe failed → conservative default (false)
	}
	return complex
}
