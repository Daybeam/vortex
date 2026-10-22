package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/daybeam/vortex/schemas"
)

// writeIfAbsent writes data to path only if the file does not already exist.
// Returns true if it wrote (file was absent), false if it skipped (file existed).
// Uses O_EXCL for an atomic create-or-skip — eliminates the TOCTOU race that
// the prior Stat→Write sequence had (audit NEW-L1, 2026-10-04).
func writeIfAbsent(path string, data []byte) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if os.IsExist(err) {
			return false, nil // file already exists — skip
		}
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

// SeedBuiltins writes built-in roles, SOPs, and skills to disk if they don't
// already exist. Called once at startup after NewRegistry and before
// BootstrapConfigSync. Existing files are never overwritten.
func (r *Registry) SeedBuiltins(rolesDir, sopsDir, skillsDir string) error {
	if err := r.seedBuiltinRoles(rolesDir); err != nil {
		return fmt.Errorf("seed roles: %w", err)
	}
	if err := r.seedBuiltinSOPs(sopsDir); err != nil {
		return fmt.Errorf("seed sops: %w", err)
	}
	if err := r.seedBuiltinSkills(skillsDir); err != nil {
		return fmt.Errorf("seed skills: %w", err)
	}
	return nil
}

// ── Built-in Role Prompts (centralized constants) ────────────────────────
// These are the compiled-in defaults. External overrides live in
// workspace/prompts/roles/<role_id>.txt and take priority when present
// (see PromptOverrides.RoleInstructions).

const sopGeneratorInstruction = `You are the SOP Generator and Capability Creation surface. Given a task description, explore the available MCP tools, design a workflow DAG, and produce a valid SOP JSON. Submit it via the update_sop tool.

You are also responsible for generating new roles when no existing role satisfies the task requirements. Role generation reads from the cookbook to synthesize high-quality role definitions with low hallucination. This is why you have AllowDynamicSkills, AllowDynamicMCPs, and Generatable privileges — you are the only builtin role that can create dynamic capabilities.

Guidelines:
1. First, enumerate all available tools and their parameter schemas.
2. Identify which tools are needed for the task.
3. Design a step-by-step DAG with clear dependencies and exit criteria.
4. Assign appropriate roles to each step. If no suitable role exists, generate one from the cookbook.
5. Output the SOP as valid JSON and submit via update_sop.

Constraints:
- You can create dynamic skills and mount dynamic MCPs (unlike task_planner).
- Generated role definitions become persistent system prompts — prioritize accuracy and low hallucination.
- Cookbook content may be large; extract only relevant engineering principles, not raw content.`

const decisionArbiterInstruction = `You are a decision arbiter for an autonomous task orchestration engine. A workflow step has blocked and requires a decision. You will receive the decision type, context, and available options. Choose exactly ONE option.

Decision guidelines:
- StepFailed: If the failure is recoverable (transient error, missing input, tool timeout), prefer retry or resume. If the failure is systemic (logic error, unrecoverable state), prefer skip to move past the failed step and preserve remaining work.
- MaxTurnsExhausted: The step used its entire turn budget without completing. Prefer resume_more_turns if the step was making progress (partial results, converging). Prefer skip if the step was spinning without progress.
- LowConfidence: The step reported low confidence in its output. Prefer skip to avoid propagating unreliable results, unless the task explicitly requires best-effort output.
- AutonomousAbortRequested: A cost-governance sentinel fired. Prefer continue if the task is near completion or the cost is within budget. Prefer confirm_abort if the task is clearly diverging or the cost is unsustainable.
- CapabilityRequired / EnvironmentMissing / DelegationRequired: These indicate missing prerequisites. Prefer skip — the step cannot succeed without the missing capability/environment/delegation.

General principles:
- Preserve completed work: prefer skip over abort when other steps have succeeded.
- Don't loop: if the same decision has been made before, choose a different option.
- Respond with ONLY the option name, nothing else.`

// taskPlannerInstruction (ADDED 2026-10-10): instruction for the task_planner
// builtin role. This role plans and modifies the DAG when the main agent is a
// weak model or the task complexity is high. It only plans — it does not execute.
// See docs/plan/CONTEXT_ARCHIVE_TOPIC_SEGMENTATION_DESIGN.md §3.2.
const taskPlannerInstruction = `You are a Task Planner for an autonomous task orchestration engine. Your job is to analyze the current task graph and produce or modify a DAG (Directed Acyclic Graph) of steps that decomposes the task into executable units.

Guidelines:
1. Analyze the task description and any existing step summaries from completed steps.
2. Identify the remaining work and decompose it into ordered steps with clear dependencies.
3. For each step, specify: role_id, task description, depends_on (predecessor step IDs), and exit_criteria.
4. Assign appropriate roles based on the work type (code, analysis, verification, etc.).
5. If modifying an existing DAG, preserve completed steps and only re-plan remaining work.
6. Keep the DAG minimal — avoid unnecessary steps. Each step should have a clear, testable outcome.
7. Output the DAG as a valid JSON task graph.

Constraints:
- You ONLY plan. You do not execute any steps or use any tools.
- You do not create roles, skills, or MCPs. Use only existing capabilities.
- If a role is missing, return an error — do not attempt to generate one. Role generation is sop_generator's responsibility.
- The DAG must be acyclic — no circular dependencies.`

// ── Built-in Roles ────────────────────────────────────────────────────────
//
// Fallback Role Semantics (Phase 3, 2026-10-20):
//
// All three builtin roles share BaseCapability="plan" but serve as FALLBACKS
// for different execution modes. They are DORMANT by default in MCP mode
// (where the main agent handles planning, decisions, and SOP creation directly)
// and activate only when the main agent is absent, weak, or non-interactive.
//
//   task_planner   — Fallback planner for weak-model / standalone mode.
//                    Activated by system.enable_planner_first (defaults false).
//                    ONLY fires for single, dependency-free step submissions
//                    (scheduler_submit.go:211). Multi-step submissions bypass
//                    it — the main agent already designed the DAG.
//                    Dormant in MCP mode when the main agent provides explicit
//                    steps via orchestrator_submit_task.
//
//   decision_arbiter — Fallback decider for NonInteractive mode (CI/batch).
//                      Activated by system.NonInteractive flag
//                      (scheduler_decision_gate.go:195). In interactive/MCP
//                      mode, decisions are exposed via pending_decisions and
//                      the main agent calls orchestrator_submit_decision.
//
//   sop_generator  — Capability creation surface, NOT a fallback planner.
//                    Generates SOPs, roles, and skills from the cookbook
//                    (RoleGenerator). Active when role generation is needed
//                    (Generatable=true → spawner_utils.go:813 triggers
//                    DYNAMIC_ESCALATION_REQUIRED). Has unique privileges
//                    (AllowDynamicSkills, AllowDynamicMCPs, Generatable)
//                    that task_planner does NOT have — different quality
//                    regime (persistent role defs vs ephemeral DAGs).
//
// See docs/completed/2026-10-21/BUILTIN_PLAN_ROLES_OVERLAP_ANALYSIS.md §5 Phase 3.

var builtinRoles = []Role{
	{
		ID:                 "sop_generator",
		Name:               "SOP Generator",
		BaseCapability:     "plan",
		Instruction:        sopGeneratorInstruction,
		AllowDynamicSkills: true,
		AllowDynamicMCPs:   true,
		Generatable:        true,
		// WHY these privileges: sop_generator is the capability creation surface.
		// It generates SOPs, roles, and skills from the cookbook (RoleGenerator).
		// Generatable=true means it can be auto-generated when missing (spawner_utils.go:813).
		// AllowDynamicSkills/MCPs=true means it can create dynamic capabilities.
		// task_planner has all three=false because it only plans — it must NOT create
		// capabilities (different quality regime: ephemeral DAG vs persistent role defs).
		// See docs/completed/2026-10-21/BUILTIN_PLAN_ROLES_OVERLAP_ANALYSIS.md §2.4.
	},
	{
		ID:             "decision_arbiter",
		Name:           "Decision Arbiter",
		BaseCapability: "plan",
		Instruction:    decisionArbiterInstruction,
		Purpose:        "Resolves blocked decisions in non-interactive mode. Spawned when a step blocks and decision_decider_role is set to this role.",
		BestFor:        "CI/batch eval, unattended deployments, autonomous decision resolution",
		MutableByAgent: false, // users can edit the file on disk, but the agent shouldn't rewrite its own decision logic
	},
	{
		ID:                 "task_planner",
		Name:               "Task Planner",
		BaseCapability:     "plan",
		Instruction:        taskPlannerInstruction,
		Purpose:            "Plans and modifies the DAG when the main agent (weak model) hits complexity threshold or is blocked.",
		BestFor:            "Weak-model fallback, complex DAG authoring, task decomposition",
		MutableByAgent:     false, // agent cannot self-modify, same as decision_arbiter
		AllowDynamicSkills: false, // only plans, does not execute — cannot create skills
		AllowDynamicMCPs:   false, // only plans, does not execute — cannot mount MCPs
		Generatable:        false, // cannot be auto-generated — it's a fixed builtin planner
		// Contrast with sop_generator: task_planner produces ephemeral one-shot DAGs,
		// not persistent role definitions. Different quality regime, different privileges.
	},
}

func (r *Registry) seedBuiltinRoles(rolesDir string) error {
	for _, role := range builtinRoles {
		if override, ok := r.PromptOverrides.RoleInstructions[role.ID]; ok && override != "" {
			role.Instruction = override
		}
		path := filepath.Join(rolesDir, role.ID+".json")
		data, err := json.MarshalIndent(role, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal role %s: %w", role.ID, err)
		}
		wrote, err := writeIfAbsent(path, data)
		if err != nil {
			return fmt.Errorf("write role %s: %w", role.ID, err)
		}
		if wrote {
			log.Printf("[seed] role %s → %s", role.ID, path)
		}
	}
	return nil
}

// ── Built-in SOPs ─────────────────────────────────────────────────────────

var builtinSOPs = []schemas.SOP{
	{
		ID:             "sop_auto_generate",
		Version:        "1.0",
		Author:         "system",
		MutableByAgent: true,
		Description:    "Self-improvement loop: given a task description, explore available MCP tools, generate a SOP, and save it.",
		Tags:           []string{"meta", "self-improvement"},
		TypicalTasks:   []string{"create a new SOP", "automate a workflow"},
		Triggers: []schemas.SOPTrigger{
			{Keywords: []string{"generate sop", "create workflow", "automate task"}},
		},
		Steps: map[string]schemas.SOPStep{
			"explore": {
				ID:     "explore",
				Role:   "sop_generator",
				Task:   "Explore all available MCP tools and their schemas. List each tool name, its parameters, and what it does.",
				Skills: []string{"code_search"},
			},
			"design": {
				ID:        "design",
				Role:      "sop_generator",
				Task:      "Based on the explored tools, design a workflow DAG for the requested task. Define steps, dependencies, roles, and exit criteria.",
				DependsOn: []string{"explore"},
			},
			"save": {
				ID:           "save",
				Role:         "sop_generator",
				Task:         "Convert the designed workflow into a valid SOP JSON and submit it via the update_sop tool.",
				DependsOn:    []string{"design"},
				ExitCriteria: "SOP saved successfully and validateSOPDAG passes",
			},
		},
	},
}

func (r *Registry) seedBuiltinSOPs(sopsDir string) error {
	for _, sop := range builtinSOPs {
		path := filepath.Join(sopsDir, sop.ID+".json")
		data, err := json.MarshalIndent(sop, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal sop %s: %w", sop.ID, err)
		}
		wrote, err := writeIfAbsent(path, data)
		if err != nil {
			return fmt.Errorf("write sop %s: %w", sop.ID, err)
		}
		if wrote {
			log.Printf("[seed] sop %s → %s", sop.ID, path)
		}
	}
	return nil
}

// ── Built-in Skills ───────────────────────────────────────────────────────

var builtinSkills = []*Skill{
	{
		ID:          "read_file",
		Name:        "Read File",
		Capability:  "code",
		Description: "Read content of a file from disk.",
		Implementations: map[string]SkillImplementation{
			"default": {
				SystemPrompt: "When you need to see the content of a file, use the available read_file or similar tool. Analyze the content carefully.",
			},
		},
	},
	{
		ID:          "code_search",
		Name:        "Code Search",
		Capability:  "code",
		Description: "Search for symbols or text across the project.",
		Implementations: map[string]SkillImplementation{
			"default": {
				SystemPrompt: "Use code search tools to find relevant files and definitions before making changes.",
			},
		},
	},
	{
		ID:          "sop_writing",
		Name:        "SOP Writing",
		Capability:  "plan",
		Description: "Guidance for generating valid SOP JSON with correct DAG structure, step dependencies, and exit criteria.",
		Implementations: map[string]SkillImplementation{
			"default": {
				SystemPrompt: "When writing a SOP, ensure: (1) each step has a unique ID, (2) depends_on references exist, (3) no cycles in the DAG, (4) exit_criteria are testable, (5) roles are defined in the registry.",
			},
		},
	},
}

func (r *Registry) seedBuiltinSkills(skillsDir string) error {
	for _, skill := range builtinSkills {
		path := filepath.Join(skillsDir, skill.ID+".json")
		data, err := json.MarshalIndent(skill, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal skill %s: %w", skill.ID, err)
		}
		wrote, err := writeIfAbsent(path, data)
		if err != nil {
			return fmt.Errorf("write skill %s: %w", skill.ID, err)
		}
		if wrote {
			log.Printf("[seed] skill %s → %s", skill.ID, path)
		}
	}
	return nil
}
