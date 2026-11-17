package schemas

import (
	"encoding/json"
	"sync"
	"time"
)

type Metadata struct {
	Namespace string      `json:"namespace"`
	Version   string      `json:"version"`
	Data      interface{} `json:"data"`

	// DeltaBacktrack marks a step as being re-executed in delta-backtrack
	// mode (supplement missing fields rather than full re-run). Set by
	// tryBacktrack before re-executing upstream; checked by prompt_assembler
	// to inject PreviousOutputSummary. Cleared on user-initiated retry.
	// See docs/plan/DELTA_BACKTRACK_DESIGN.md.
	DeltaBacktrack bool `json:"delta_backtrack,omitempty"`
}

// ─── Step ─────────────────────────────────────────────────────────────────

type StepInput struct {
	ID                       string              `json:"id"`
	RoleID                   string              `json:"role_id"`
	SOPRef                   string              `json:"sop_ref,omitempty"`
	Task                     string              `json:"task"`
	DependsOn                []string            `json:"depends_on"`
	AdditionalSkills         []string            `json:"additional_skills"`
	AdditionalMCPs           []string            `json:"additional_mcps"`
	AdditionalToolAllowlists map[string][]string `json:"additional_tool_allowlists"`
	ContextRefs              map[string]string   `json:"context_refs"`
	SubmitterIsAdmin         bool                `json:"-"` // set by entry handler; engine copies to TaskGraph (json:"-" ⇒ unforgeable)
	OwnerID                  string              `json:"-"` // set by entry handler from AuthOwnerKey; engine copies to TaskGraph (json:"-" ⇒ unforgeable)
	ProjectID                string              `json:"project_id,omitempty"` // project space; defaults to "default" if empty
	FallbackFor              string              `json:"fallback_for,omitempty"`
	MaxRetries               int                 `json:"max_retries"`

	// ProviderOverride allows a step to use a different named provider than the role default.
	// Must match a key in config.json "providers". Leave empty to use role/system default.
	ProviderOverride string `json:"provider,omitempty"`
	RoutingMode      string `json:"routing_mode,omitempty"` // legacy | manifest

	// Optimization & Audit
	ExitCriteria  string  `json:"exit_criteria,omitempty"`
	VerifierModel string  `json:"verifier_model,omitempty"`
	MaxAutoRefine int     `json:"max_auto_refine,omitempty"`
	RiskRating    float64 `json:"risk_rating,omitempty"` // Numeric risk score (0.0-1.0)
	// DeterministicChecks mirrors Step.DeterministicChecks below; until it existed
	// here no SOP or submit body could reach Case 2.5 (register O1).
	DeterministicChecks []DeterministicCheck `json:"deterministic_checks,omitempty"`
	// Cross-Family Debate (ADDED 2026-09-14): opt-in Proposer→Critic→Synthesizer loop.
	// See docs/architecture/CROSS_FAMILY_DEBATE_DESIGN.md
	EnableDebate    bool `json:"enable_debate,omitempty"`
	MaxDebateRounds int  `json:"max_debate_rounds,omitempty"` // hard-capped at 2
	// EnableStepPlan (ADDED 2026-10-10): see Step.EnableStepPlan.
	EnableStepPlan bool `json:"enable_step_plan,omitempty"`
	// DynamicRubrics are task-specific audit constraints (Phase 2 of Self-Evolution).
	// If empty, the engine may generate them dynamically during audit.
	DynamicRubrics []string `json:"dynamic_rubrics,omitempty"`

	// OutputContract is the deterministic schema that the step's artifact MUST
	// satisfy before downstream handoff. Declared by the Main Agent when it
	// builds the DAG — not by the sub-agent. If empty, no contract check runs.
	//
	// Artifact Constraint Shield (arXiv:2608.24569)
	OutputContract ArtifactContract `json:"output_contract,omitempty"`

	// Deliver specifies artifact delivery destination/method (ADDED 2026-08-30)
	Deliver string `json:"deliver,omitempty"`

	// GroupID, when non-empty, records which RoleGroup produced this step.
	// Set automatically by GroupDispatcher; callers need not set this manually.
	GroupID string `json:"group_id,omitempty"`

	// FailurePolicy is an optional, additive per-step override of how this
	// step's engine-recognized permanent failures are handled -- see
	// FailurePolicy's own doc comment below and core/failure_classify.go.
	// Leaving this unset (nil, the default) preserves the exact previous
	// behavior for every existing step/caller.
	FailurePolicy   *FailurePolicy `json:"failure_policy,omitempty"`
	CompressionHint string         `json:"compression_hint,omitempty"`

	SpawnDepth    int `json:"spawn_depth,omitempty"`
	MaxSpawnDepth int `json:"max_spawn_depth,omitempty"`

	// MaxLoopRounds bounds how many times a downstream step may trigger
	// upstream backtracking via MissingContext before the cycle breaker
	// fuses. 0 = disabled (not recommended — weak models may loop forever).
	// See docs/architecture/CYCLE_BREAKER_DESIGN.md.
	MaxLoopRounds int `json:"max_loop_rounds,omitempty"`

	// InputDir declares a directory (relative to the session WorkspaceRoot)
	// whose contents should be auto-copied into the task workspace at start.
	// See docs/completed/2026-09-19/WORKSPACE_AND_SANDBOX_REDESIGN.md §2.2 ③.
	// Empty = no input seeding (backward compatible).
	InputDir string `json:"input_dir,omitempty"`

	// EvoX-inspired Zero-Loss Coordination (ADDED 2026-08-17)
	MergeStrategy string            `json:"merge_strategy,omitempty"` // append | unique | overlay
	TargetKey     string            `json:"target_key,omitempty"`     // Key in GlobalWorkspace
	InputMapping  map[string]string `json:"input_mapping,omitempty"`  // arg_name -> step_id.field
	Isolation     bool              `json:"isolation,omitempty"`      // If true, skip historical summaries
}

// UnmarshalJSON implements custom unmarshaling for StepInput to support
// "role" as an alias for "role_id". This improves compatibility with
// external callers (e.g. browser extensions or simple API clients) that
// use the more intuitive "role" key.
func (s *StepInput) UnmarshalJSON(data []byte) error {
	type Alias StepInput
	aux := &struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		Objective string `json:"objective"`
		Goal      string `json:"goal"`
		*Alias
	}{
		Alias: (*Alias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if s.RoleID == "" && aux.Role != "" {
		s.RoleID = aux.Role
	}
	if s.Task == "" {
		if aux.Content != "" {
			s.Task = aux.Content
		} else if aux.Objective != "" {
			s.Task = aux.Objective
		} else if aux.Goal != "" {
			s.Task = aux.Goal
		}
	}
	return nil
}

// FailurePolicy declares how a step wants specific classes of engine-
// recognized failure to be handled, as an alternative to the scheduler's
// default (always block on a decision_required event). This was introduced
// in the 2026-07-24 transaction-design discussion as the "soft requirement"
// answer: rather than a hard, all-or-nothing transactional guarantee around
// step side effects (which classic ACID semantics can't meaningfully
// provide for arbitrary external tool calls -- an already-sent email can't
// be rolled back), each step can declare its own preference for how
// specific, well-understood failure classes should resolve.
//
// A nil FailurePolicy (the default) changes nothing: every failure path
// continues to behave exactly as it did before this field existed.
type FailurePolicy struct {
	// OnFailure overrides what happens when this step hits
	// FailureClassRoleMissing or FailureClassMissingDependency (see
	// core/failure_classify.go for the full failure taxonomy). Recognized
	// values:
	//   "abort" -- immediately mark the step and graph as blocked and stop,
	//              without waiting for a human/caller decision.
	//   "skip"  -- immediately mark the step skipped and let the rest of
	//              the graph continue.
	// Any other value, including the empty string, falls through to the
	// existing decision_required behavior (skip/abort offered as options,
	// caller must explicitly choose via orchestrator_submit_decision).
	OnFailure string `json:"on_failure,omitempty"`
}

// Step is one node in the task graph.
type Step struct {
	ID        string    `json:"id"`
	RoleID    string    `json:"role_id"`
	Task      string    `json:"task"`
	DependsOn []string  `json:"depends_on"`
	ReadDeps  []ReadDep `json:"read_deps,omitempty"`

	AdditionalSkills         []string            `json:"additional_skills"`
	AdditionalMCPs           []string            `json:"additional_mcps"`
	AdditionalToolAllowlists map[string][]string `json:"additional_tool_allowlists"`
	ContextRefs              map[string]string   `json:"context_refs"`
	FallbackFor              string              `json:"fallback_for,omitempty"`

	// ProviderOverride: step-level provider, takes precedence over role default.
	ProviderOverride string `json:"provider,omitempty"`
	RoutingMode      string `json:"routing_mode,omitempty"` // legacy | manifest

	// Optimization & Audit state
	ExitCriteria    string  `json:"exit_criteria,omitempty"`
	VerifierModel   string  `json:"verifier_model,omitempty"`
	MaxAutoRefine   int     `json:"max_auto_refine,omitempty"`
	AutoRefineCount int     `json:"auto_refine_count"`
	RiskRating      float64 `json:"risk_rating"` // Captured numeric risk score
	// Cross-Family Debate (ADDED 2026-09-14): opt-in Proposer→Critic→Synthesizer loop.
	// See docs/architecture/CROSS_FAMILY_DEBATE_DESIGN.md
	EnableDebate    bool `json:"enable_debate,omitempty"`
	MaxDebateRounds int  `json:"max_debate_rounds,omitempty"` // hard-capped at 2

	// EnableStepPlan (ADDED 2026-10-10): forces this Step through a Planner
	// Phase before execution. The Planner generates a markdown plan using
	// read-only tools; the Executor then receives the plan as context.
	// See docs/plan/STEP_PLAN_MODE_DESIGN.md.
	EnableStepPlan bool `json:"enable_step_plan,omitempty"`

	// OutputContract mirrors StepInput.OutputContract — the deterministic schema
	// this step's artifact must satisfy. Set at Submit time, immutable after.
	// Artifact Constraint Shield (arXiv:2608.24569)
	OutputContract ArtifactContract `json:"output_contract,omitempty"`

	// Deliver specifies artifact delivery destination/method (ADDED 2026-08-30)
	Deliver string `json:"deliver,omitempty"`

	// TargetFile (ADDED 2026-09-13) — declares the primary file path this step
	// writes to. Used by PathLockManager to acquire a write lock preventing
	// concurrent steps from clobbering the same file. Empty = no lock needed.
	TargetFile string `json:"target_file,omitempty"`

	// DeterministicChecks (ADDED 2026-09-13) — zero-token hard verification
	// runs before any VerifierModel semantic audit. See core/deterministic_verifier.go.
	DeterministicChecks []DeterministicCheck `json:"deterministic_checks,omitempty"`

	// Runtime state — mutated by scheduler
	Status     StepStatus `json:"status"`
	RetryCount int        `json:"retry_count"`
	MaxRetries int        `json:"max_retries"`
	// TurnsBudgetBonus is extra tool-turn budget granted to a step on top of
	// maxTurns, accumulated by repeated "resume_more_turns" decisions. It is how
	// a step genuinely resumes with more room instead of re-running the same
	// capped loop and hitting the wall again. ADDED (2026-09-07).
	TurnsBudgetBonus int       `json:"turns_budget_bonus,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	TriggerError     string    `json:"trigger_error,omitempty"`  // First error that led to successful retry (ADDED 2026-08-16)
	PartialOutput    string    `json:"partial_output,omitempty"` // B1: model's last output before Sieve interception; passed to auditor/next step as context
	ResultRef        string    `json:"result_ref,omitempty"`
	Confidence       *float64  `json:"confidence,omitempty"`
	MissingCtx       []string  `json:"missing_context,omitempty"`
	ProviderID       string    `json:"provider_id,omitempty"` // Effective Provider ID
	ModelID          string    `json:"model_id,omitempty"`    // Effective Model ID
	Metadata         *Metadata `json:"metadata,omitempty"`
	LastWorkedOn     time.Time `json:"last_worked_on,omitempty"`
	// GroupID records which RoleGroup expanded into this step (observability only).
	GroupID string `json:"group_id,omitempty"`

	// FailurePolicy: see the FailurePolicy type doc comment above (StepInput
	// section). Propagated from StepInput at graph-build time; nil means
	// "use default behavior", unchanged from before this field existed.
	FailurePolicy   *FailurePolicy `json:"failure_policy,omitempty"`
	CompressionHint string         `json:"compression_hint,omitempty"`

	// RequireHumanApproval (ADDED 2026-09-08): step requires a decision
	RequireHumanApproval bool `json:"require_human_approval,omitempty"`

	// ODFTP-native recovery context (ADDED 2026-08-30)
	AdditionalPromptContext string `json:"additional_prompt_context,omitempty"`
	// PreviousOutputSummary caches the upstream step's previous output
	// summary for delta-backtrack injection. Set by tryBacktrack before
	// re-executing upstream. Cleared when the step is fully re-executed
	// by a user-initiated retry. ADDED (2026-10-17).
	// See docs/plan/DELTA_BACKTRACK_DESIGN.md.
	PreviousOutputSummary string `json:"previous_output_summary,omitempty"`

	// Perception 2.0
	Embedding      []float32 `json:"embedding,omitempty"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`

	// Async task state (ADDED 2026-09-11): set when step is suspended for async polling.
	AsyncHandleRef string `json:"async_handle_ref,omitempty"` // e.g. "backend:jobID"

	SpawnDepth    int `json:"spawn_depth,omitempty"`
	MaxSpawnDepth int `json:"max_spawn_depth,omitempty"`
	MaxLoopRounds int `json:"max_loop_rounds,omitempty"` // cycle breaker fuse limit

	// InputDir declares a directory (relative to the session WorkspaceRoot)
	// whose contents should be auto-copied into the task workspace at start.
	// Empty = no input seeding (backward compatible).
	InputDir string `json:"input_dir,omitempty"`

	// DynamicRubrics: persistent audit constraints (Phase 2 of Self-Evolution).
	DynamicRubrics []string `json:"dynamic_rubrics,omitempty"`

	// PGPO: Visited environment states (ADDED 2026-09-08)
	StatesVisited []string `json:"states_visited,omitempty"`

	// EvoX-inspired Zero-Loss Coordination (ADDED 2026-08-17)
	MergeStrategy string            `json:"merge_strategy,omitempty"`
	TargetKey     string            `json:"target_key,omitempty"`
	InputMapping  map[string]string `json:"input_mapping,omitempty"`
	Isolation     bool              `json:"isolation,omitempty"`

	// ExecutionProtocol (ADDED 2026-10-26): selects the execution backend for
	// this step. "" or "internal" = standard Spawner LLM loop (default).
	// "acp" = delegate to an external ACP-compliant worker via JSON-RPC.
	// See docs/completed/2026-10-28/ACP_CLIENT_SPAWNER_INTEGRATION_DESIGN.md.
	ExecutionProtocol string         `json:"execution_protocol,omitempty"`
	ACPConfig         *ACPStepConfig `json:"acp_config,omitempty"`
}

// ACPStepConfig configures an external ACP (Agent Client Protocol) worker
// for step execution. Only used when Step.ExecutionProtocol == "acp".
// See docs/completed/2026-10-28/ACP_CLIENT_SPAWNER_INTEGRATION_DESIGN.md §3.1.
type ACPStepConfig struct {
	BinaryPath string   `json:"binary_path"`
	Args       []string `json:"args,omitempty"`
	WorkDir    string   `json:"work_dir,omitempty"`
	TimeoutSec int      `json:"timeout_sec,omitempty"`
}

func (s *Step) IsTerminal() bool {
	switch s.Status {
	case StepOK, StepPartial, StepFailed, StepSkipped, StepBlocked, StepDeprecated:
		return true
	}
	return false
}

// ─── Decision ─────────────────────────────────────────────────────────────

type Decision struct {
	ID      string         `json:"id"`
	StepID  string         `json:"step_id"`
	Type    DecisionType   `json:"type"`
	Context map[string]any `json:"context"`
	Options []string       `json:"options"`
	Created time.Time      `json:"created_at"`
}

// DecisionNode represents a structured autonomous decision made by the agent.
// Used for causal auditing and semantic precedent retrieval.
type DecisionNode struct {
	ID        string    `json:"id"`
	StepID    string    `json:"step_id"`
	Turn      int       `json:"turn"`
	Type      string    `json:"type"`      // routing|selection|verification|synthesis
	Reasoning string    `json:"reasoning"` // LLM's raw thought or summarized rationale
	Action    string    `json:"action"`    // Tool call name or final status
	Outcome   string    `json:"outcome"`   // Summary of the result
	Causes    []string  `json:"causes"`    // References to upstream step IDs or data keys
	Timestamp time.Time `json:"timestamp"`

	// Perception 2.0 (Semantic Retrieval)
	Embedding      []float32 `json:"embedding,omitempty"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`

	// Memory tiering & observability (design §1.4 scheme B, 2026-10-23):
	// Source classifies whether this decision is user-confirmed, model-guessed,
	// or verifier-audited — so retrieval can rank trusted decisions above
	// speculative ones. Confidence is the 0.0-1.0 certainty at decision time.
	Source     string  `json:"source,omitempty"`     // "user" | "model" | "verifier"
	Confidence float64 `json:"confidence,omitempty"` // 0.0-1.0
	Domain     string  `json:"domain,omitempty"`     // topic facet (design §4: frozen at write time)
}

// ─── OutputFile ───────────────────────────────────────────────────────────

// ArtifactContract is the delivery contract for a task artifact: provenance,
// integrity (SHA-256), and delivery state. Written alongside the task manifest
// so downstream consumers can read structured facts instead of guessing from text.
type ArtifactContract struct {
	Path        string    `json:"path"`
	SourceRefs  []string  `json:"source_refs,omitempty"` // upstream evidence/step refs
	SHA256      string    `json:"sha256,omitempty"`
	Status      string    `json:"status"` // draft|validated|published
	Format      string    `json:"format,omitempty"`
	SizeBytes   int       `json:"size_bytes,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
	OwnerStep   string    `json:"owner_step,omitempty"`

	// DeliveryStatus tracks artifact handoff state (ADDED 2026-08-30)
	DeliveryStatus string `json:"delivery_status,omitempty"`

	// ─── Artifact Constraint Shield (arXiv:2608.24569) ───────────────────
	RequiredFields []string          `json:"required_fields,omitempty"`
	TypeSchema     map[string]string `json:"type_schema,omitempty"` // field -> type (string, number, array, object)
}

// ReadDep records an actual file-level read dependency for coordination tracking.
// One step's trace may reveal that it read a file produced by another step,
// forming an AGENT -> FILE -> AGENT edge in the coordination graph.
type OutputFile struct {
	StepID    string    `json:"step_id"`
	Path      string    `json:"path"`
	Format    string    `json:"format"`
	Summary   string    `json:"summary"`
	SizeBytes int       `json:"size_bytes"`
	IsPrimary bool      `json:"is_primary"`
	Deliver   string    `json:"deliver,omitempty"` // ADDED 2026-08-30
	CreatedAt time.Time `json:"created_at"`
	Metadata  *Metadata `json:"metadata,omitempty"`
}

// ─── Context Tree ─────────────────────────────────────────────────────────

// NodeStatus represents the lifecycle of a context node.
type NodeStatus string

const (
	NodeActive    NodeStatus = "active"
	NodeResolved  NodeStatus = "resolved"
	NodeAbandoned NodeStatus = "abandoned"
)

// ContextNode represents a snapshot of the conversation/task state.
// This allows for branching and folding of context.
type ContextNode struct {
	ID       string         `json:"id"`
	ParentID string         `json:"parent_id,omitempty"`
	Intent   string         `json:"intent"`
	Goal     string         `json:"goal"`
	Status   NodeStatus     `json:"status"`
	Summary  string         `json:"summary,omitempty"`
	Checksum string         `json:"checksum,omitempty"` // For Folding integrity
	StepIDs  []string       `json:"step_ids"`           // Steps that contributed to this node
	Metadata map[string]any `json:"metadata,omitempty"`

	// Context Isolation
	LocalSymbolIndex map[string]any `json:"local_symbol_index,omitempty"`

	// Semantic Data
	Embedding      []float32 `json:"embedding,omitempty"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`
}

// ─── TaskGraph ────────────────────────────────────────────────────────────

type TaskGraph struct {
	TaskID  string           `json:"task_id"`
	TraceID string           `json:"trace_id,omitempty"`
	// ProjectID identifies the project space this task belongs to. Tasks
	// sharing the same ProjectID can recall each other's ScopeProject memory
	// items. Defaults to "default" when unspecified (backward-compatible).
	ProjectID string           `json:"project_id,omitempty"`
	Steps     map[string]*Step `json:"steps"`
	Status  GraphStatus      `json:"status"`
	// SubmitterIsAdmin is the authoritative mount-gate source: read via
	// core/mount_gate.go's effectiveSubmitterIsAdmin, enforced in
	// core/validator.go's ResolveFinalMCPs. Persisted so fork/reload retain the tier.
	SubmitterIsAdmin bool `json:"submitter_is_admin,omitempty"`
	// ApprovedMCPMounts: MCP ids a human approved (G2); persisted across reload.
	ApprovedMCPMounts   map[string]*ApprovalRecord `json:"approved_mcp_mounts,omitempty"`
	ApprovedSkillMounts map[string]*ApprovalRecord `json:"approved_skill_mounts,omitempty"` // skill ids a human approved (D3, 2026-10-22); persisted
	PendingDecisions    []*Decision                `json:"pending_decisions"`
	OutputFiles         []OutputFile               `json:"output_files"`
	Artifacts           []ArtifactContract         `json:"artifacts,omitempty"`
	OverallConfidence   *float64                   `json:"overall_confidence,omitempty"`
	CreatedAt           time.Time                  `json:"created_at"`
	CompletedAt         *time.Time                 `json:"completed_at,omitempty"`

	// Tree-based context management
	ContextTree   map[string]*ContextNode `json:"context_tree,omitempty"`
	CurrentNodeID string                  `json:"current_node_id,omitempty"`

	// Session-scoped execution IR (Ephemeral Roles/Skills)
	// These are stored as map[string]any to avoid circular dependency with config package.
	// In practice, they hold *config.Role and *config.Skill objects.
	SessionRoles     map[string]any `json:"session_roles,omitempty"`
	SessionSkills    map[string]any `json:"session_skills,omitempty"`
	SessionProviders map[string]any `json:"session_providers,omitempty"`

	// GlobalWorkspace holds structured data for programmatic merging (EvoX)
	GlobalWorkspace map[string]any `json:"global_workspace,omitempty"`

	// MainProviderID optionally identifies the session's primary model, used as a
	// universal fallback for all steps if their specific provider fails.
	MainProviderID string `json:"main_provider_id,omitempty"`

	// Coordination Tracking (v3.8 arXiv:2608.16801 inspired)
	CoordinationEdges []CoordinationEdge `json:"coordination_edges,omitempty"`

	// Token Budgeting (ADDED 2026-08-30)
	TokenBudget  int64 `json:"token_budget,omitempty"`
	TokensUsed   int64 `json:"tokens_used,omitempty"`
	BudgetPaused bool  `json:"budget_paused,omitempty"`

	// TimeoutSecs is the per-task timeout in seconds. 0 = use system default.
	// ADDED (2026-09-19): see DELEGATION_AND_SUBMIT_DEFECTS.md §2.3.
	TimeoutSecs int `json:"timeout_secs,omitempty"`

	// ConsecutiveSkips tracks how many decisions in a row were auto-resolved
	// as "skip". When this reaches MaxConsecutiveSkips, "skip" is filtered
	// out of subsequent decision options to prevent silent cascade through
	// the remaining DAG. Reset on any non-skip choice.
	ConsecutiveSkips int `json:"consecutive_skips,omitempty"`

	// DecisionHistory tracks structured autonomous reasoning points.
	// ADDED (2026-08-26): see Priority 5 roadmap.
	DecisionHistory map[string]*DecisionNode `json:"decision_history,omitempty"`

	// sessionMu guards SessionRoles/SessionSkills/SessionProviders (2026-07-20).
	sessionMu sync.Mutex `json:"-"`

	// decisionMu guards DecisionHistory (P-2.1 Step 2: decoupled from
	// DirectedEngine.Mu so Spawner DecisionHistory writes don't block
	// all graph operations across all tasks).
	decisionMu sync.RWMutex `json:"-"`

	// Session-level workspace binding (ADDED 2026-09-19).
	// See docs/completed/2026-09-19/WORKSPACE_AND_SANDBOX_REDESIGN.md §2.2 ②.
	// When SessionID is empty, behavior is identical to legacy (backward compatible).
	SessionID     string `json:"session_id,omitempty"`
	WorkspaceRoot string `json:"workspace_root,omitempty"`

	// OwnerID is set at submission time from the caller's resolved identity
	// (tools.AuthOwnerKey). When empty (no APIKeys configured), all ownership
	// checks are no-ops — backward compatible with single-operator deployments.
	// See docs/completed/2026-10-02/architecture-task-ownership.md §2.3.
	OwnerID string `json:"owner_id,omitempty"`

	IsSmartRouted bool `json:"is_smart_routed,omitempty"`

	// Time Travel & Fork (ADDED 2026-09-14): records task lineage for
	// counterfactual branching. See docs/architecture/TIME_TRAVEL_AND_OBSERVABILITY_DESIGN.md
	ParentTaskID string `json:"parent_task_id,omitempty"` // if forked, points to source task
	ForkedAtStep string `json:"forked_at_step,omitempty"` // step ID where fork occurred

	// Task-Local Closure (ADDED 2026-09-15): snapshots JIT MCP definitions
	// referenced by this task's steps at submission time. If a JIT tool's
	// global TTL expires mid-task, executeStep restores it from this snapshot
	// so the task never hits "tool not found". See
	// docs/architecture/TASK_LOCAL_CLOSURE_AND_FALLBACK_DESIGN.md §2.1.
	// Stored as map[string]any (JSON-serialized *config.MCPDef) to avoid a
	// circular dependency with the config package — same pattern as
	// SessionRoles/SessionSkills above.
	LocalMCPs map[string]json.RawMessage `json:"local_mcps,omitempty"`

	// CycleCounters tracks active local-loop backtracking rounds.
	// Key = cycle signature (sorted participant step IDs joined by ":")
	// Value = current round count. Removed on fuse.
	// See docs/architecture/CYCLE_BREAKER_DESIGN.md.
	CycleCounters map[string]int `json:"cycle_counters,omitempty"`

	// Substitutions records every instance where actual execution differed from
	// the request (role fallback, MCP degradation, etc.). Surfaced in
	// get_task_status so drift is visible without log mining.
	// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P3.
	Substitutions []Substitution `json:"substitutions,omitempty"`

	// Charter captures the task's intent, non-goals, and constraints.
	// Optional — nil means "unspecified" and behavior is identical to
	// pre-charter tasks (backward compatible).
	// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P1.
	Charter *TaskCharter `json:"charter,omitempty"`

	// substitutionMu guards Substitutions append.
	substitutionMu sync.Mutex `json:"-"`
}

// CoordinationEdge represents a directed flow of information between steps/agents.
type CoordinationEdge struct {
	SourceID  string  `json:"source"`  // Step ID that produced the data
	TargetID  string  `json:"target"`  // Step ID that consumed the data
	Type      string  `json:"type"`    // mapping | reference | shared_vfs
	Payload   int64   `json:"payload"` // Estimated size in bytes/tokens
	Timestamp float64 `json:"ts"`      // Unix nano timestamp
}
