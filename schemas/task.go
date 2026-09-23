package schemas

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// StepStatus represents the lifecycle state of a single step.
type StepStatus string

const (
	StepPending    StepStatus = "pending"
	StepRunning    StepStatus = "running"
	StepOK         StepStatus = "ok"
	StepPartial    StepStatus = "partial"
	StepFailed     StepStatus = "failed"
	StepBlocked    StepStatus = "blocked"
	StepSkipped    StepStatus = "skipped"
	StepDeprecated StepStatus = "deprecated" // ADDED (2026-09-07): DAG surgery - Dead-end node
	StepSuspended  StepStatus = "suspended"  // ADDED (2026-09-11): async task suspended, awaiting external signal
)

// GraphStatus represents the overall task graph lifecycle.
type GraphStatus string

const (
	GraphRunning            GraphStatus = "running"
	GraphPendingReview      GraphStatus = "pending_review"
	GraphBlocked            GraphStatus = "blocked"
	GraphCompleted          GraphStatus = "completed"
	GraphCompletedWithSkips GraphStatus = "completed_with_skips"
	GraphFailed             GraphStatus = "failed"
	GraphCancelled          GraphStatus = "cancelled"
)

// DecisionType categorises why a step was blocked.
type DecisionType string

const (
	DecisionStepFailed               DecisionType = "step_failed"
	DecisionCapabilityRequired       DecisionType = "capability_required"
	DecisionLowConfidence            DecisionType = "low_confidence"
	DecisionEnvironmentMissing       DecisionType = "environment_missing"
	DecisionDelegationRequired       DecisionType = "delegation"
	DecisionUpstreamInsufficient     DecisionType = "upstream_insufficient" // ADDED (2026-08-27): upstream artifact missing or insufficient
	DecisionBudgetExhausted          DecisionType = "budget_exhausted"      // ADDED (2026-08-30): token budget exceeded
	DecisionMaxTurnsExhausted        DecisionType = "max_turns_exhausted"   // ADDED (2026-09-07): tool-turn budget exceeded; resumable
	DecisionRewriteDAG               DecisionType = "rewrite_dag"           // ADDED (2026-09-07): Dynamic DAG surgery
	DecisionHumanApprovalRequired    DecisionType = "human_approval_required"
	DecisionAutonomousAbortRequested DecisionType = "autonomous_abort_requested" // ADDED (2026-09-14): cost governance — Main Agent requests abort, user decides
)

// VerificationFailureType categorises why a verification failed.
type VerificationFailureType string

const (
	FailureNone            VerificationFailureType = ""
	FailureTimeout         VerificationFailureType = "timeout"
	FailurePermission      VerificationFailureType = "permission"
	FailureConflict        VerificationFailureType = "conflict"
	FailureLogicError      VerificationFailureType = "logic_error"
	FailureSchemaViolation VerificationFailureType = "schema_violation"
)

// DeterministicCheck defines a zero-token hard verification step.
// Runs before any VerifierModel semantic audit in verifyExitCriteria.
// See core/deterministic_verifier.go and docs/TIERED_SHORT_CIRCUIT_AUDIT.md
type DeterministicCheck struct {
	Type   string         `json:"type"`   // "file_exists", "command_pass"
	Params map[string]any `json:"params"` // verifier-specific parameters
}

const (
	RoutingModeLegacy   = "legacy"
	RoutingModeManifest = "manifest"
)

// ── Risk Tiers & Adaptive Verifier Policy (ADDED 2026-09-07) ─────────────
type RiskTier string

const (
	RiskTierLight    RiskTier = "light"    // Read-only, deterministic (No LLM audit)
	RiskTierModerate RiskTier = "moderate" // Multi-step code/data (Regex / light check)
	RiskTierCritical RiskTier = "critical" // Browser, side-effects, asset gen (Full LLM Auditor + Auto-Refine)
)

// DetermineRiskTier analyzes the task intent and bound MCPs to classify risk.
// Returns both the RiskTier label and a numeric RiskRating (0.0 - 1.0).
func DetermineRiskTier(task string, mcpList []string) (RiskTier, float64) {
	lower := strings.ToLower(task)
	for _, mcp := range mcpList {
		if mcp == "ghost-driver" {
			return RiskTierCritical, 0.95
		}
	}
	if strings.Contains(lower, "browser") || strings.Contains(lower, "image") ||
		strings.Contains(lower, "generate") || strings.Contains(lower, "scrape") ||
		strings.Contains(lower, "screenshot") || strings.Contains(lower, "pollinations") ||
		strings.Contains(lower, "deploy") || strings.Contains(lower, "payment") {
		return RiskTierCritical, 0.9
	}
	if strings.Contains(lower, "patch") || strings.Contains(lower, "refactor") ||
		strings.Contains(lower, "build") || strings.Contains(lower, "test") {
		return RiskTierModerate, 0.5
	}
	return RiskTierLight, 0.1
}

type Metadata struct {
	Namespace string      `json:"namespace"`
	Version   string      `json:"version"`
	Data      interface{} `json:"data"`
}

// ─── Step ─────────────────────────────────────────────────────────────────

type StepInput struct {
	ID                       string              `json:"id"`
	RoleID                   string              `json:"role_id"`
	Task                     string              `json:"task"`
	DependsOn                []string            `json:"depends_on"`
	AdditionalSkills         []string            `json:"additional_skills"`
	AdditionalMCPs           []string            `json:"additional_mcps"`
	AdditionalToolAllowlists map[string][]string `json:"additional_tool_allowlists"`
	ContextRefs              map[string]string   `json:"context_refs"`
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
	// Cross-Family Debate (ADDED 2026-09-14): opt-in Proposer→Critic→Synthesizer loop.
	// See docs/architecture/CROSS_FAMILY_DEBATE_DESIGN.md
	EnableDebate    bool `json:"enable_debate,omitempty"`
	MaxDebateRounds int  `json:"max_debate_rounds,omitempty"` // hard-capped at 2
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
	TriggerError     string    `json:"trigger_error,omitempty"` // First error that led to successful retry (ADDED 2026-08-16)
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
}

func (s *Step) IsTerminal() bool {
	switch s.Status {
	case StepOK, StepPartial, StepFailed, StepSkipped, StepBlocked:
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
type ReadDep struct {
	StepID       string `json:"step_id"`                 // upstream step that produced the file
	FilePath     string `json:"file_path"`               // path of the file that was read
	Evidence     string `json:"evidence"`                // trace entry that triggered the detection (e.g. "tool:read_file arg:path")
	SourceSHA256 string `json:"source_sha256,omitempty"` // content hash at time of read (StagedWorkspace only)
	SnapshotID   string `json:"snapshot_id,omitempty"`   // workspace snapshot ID (StagedWorkspace only)
}

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
	TaskID            string             `json:"task_id"`
	TraceID           string             `json:"trace_id,omitempty"`
	Steps             map[string]*Step   `json:"steps"`
	Status            GraphStatus        `json:"status"`
	PendingDecisions  []*Decision        `json:"pending_decisions"`
	OutputFiles       []OutputFile       `json:"output_files"`
	Artifacts         []ArtifactContract `json:"artifacts,omitempty"`
	OverallConfidence *float64           `json:"overall_confidence,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	CompletedAt       *time.Time         `json:"completed_at,omitempty"`

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

	// DecisionHistory tracks structured autonomous reasoning points.
	// ADDED (2026-08-26): see Priority 5 roadmap.
	DecisionHistory map[string]*DecisionNode `json:"decision_history,omitempty"`

	// sessionMu guards SessionRoles/SessionSkills/SessionProviders (2026-07-20).
	sessionMu sync.Mutex `json:"-"`

	// Session-level workspace binding (ADDED 2026-09-19).
	// See docs/completed/2026-09-19/WORKSPACE_AND_SANDBOX_REDESIGN.md §2.2 ②.
	// When SessionID is empty, behavior is identical to legacy (backward compatible).
	SessionID     string `json:"session_id,omitempty"`
	WorkspaceRoot string `json:"workspace_root,omitempty"`

	IsSmartRouted bool `json:"is_smart_routed,omitempty"`

	// Time Travel & Fork (ADDED 2026-09-14): records task lineage for
	// counterfactual branching. See docs/architecture/TIME_TRAVEL_AND_OBSERVABILITY_DESIGN.md
	ParentTaskID string `json:"parent_task_id,omitempty"` // if forked, points to source task
	ForkedAtStep string `json:"forked_at_step,omitempty"` // step ID where fork occurred

	// Task-Local Closure (ADDED 2026-09-15): snapshots JIT MCP definitions
	// referenced by this task's steps at submission time. If a JIT tool's
	// global TTL expires mid-task, executeStep restores it from this snapshot
	// so the task never hits "tool not found". See
	// docs/architecture/TASK_LOCAL_CLOSURE_AND_FALLBACK_DESIGN.md §二.1.
	// Stored as map[string]any (JSON-serialized *config.MCPDef) to avoid a
	// circular dependency with the config package — same pattern as
	// SessionRoles/SessionSkills above.
	LocalMCPs map[string]json.RawMessage `json:"local_mcps,omitempty"`

	// CycleCounters tracks active local-loop backtracking rounds.
	// Key = cycle signature (sorted participant step IDs joined by ":")
	// Value = current round count. Removed on fuse.
	// See docs/architecture/CYCLE_BREAKER_DESIGN.md.
	CycleCounters map[string]int `json:"cycle_counters,omitempty"`
}

// CoordinationEdge represents a directed flow of information between steps/agents.
type CoordinationEdge struct {
	SourceID  string  `json:"source"`  // Step ID that produced the data
	TargetID  string  `json:"target"`  // Step ID that consumed the data
	Type      string  `json:"type"`    // mapping | reference | shared_vfs
	Payload   int64   `json:"payload"` // Estimated size in bytes/tokens
	Timestamp float64 `json:"ts"`      // Unix nano timestamp
}

// GetSessionRole returns the raw stored value for a session-scoped role (either
// an already-typed *config.Role or a map[string]any, depending on whether the
// graph was freshly created or restored from a persisted manifest), and
// whether it exists. Safe for concurrent use across the parallel step
// goroutines DirectedEngine.executeStep can spawn for one graph.
func (g *TaskGraph) GetSessionRole(id string) (any, bool) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionRoles == nil {
		return nil, false
	}
	v, ok := g.SessionRoles[id]
	return v, ok
}

// SetSessionRole stores a value for a session-scoped role id -- typically used
// to cache back an already-unmarshaled *config.Role after converting a raw
// map[string]any entry, so later lookups skip the JSON round-trip. Safe for
// concurrent use.
func (g *TaskGraph) SetSessionRole(id string, v any) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionRoles == nil {
		g.SessionRoles = make(map[string]any)
	}
	g.SessionRoles[id] = v
}

// GetSessionSkill / SetSessionSkill: same contract as the Role variants above,
// for SessionSkills.
func (g *TaskGraph) GetSessionSkill(id string) (any, bool) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionSkills == nil {
		return nil, false
	}
	v, ok := g.SessionSkills[id]
	return v, ok
}

func (g *TaskGraph) SetSessionSkill(id string, v any) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionSkills == nil {
		g.SessionSkills = make(map[string]any)
	}
	g.SessionSkills[id] = v
}

// ForEachSessionSkill executes a callback for each session-scoped skill.
// The callback receives the skill ID and its raw value (map[string]any or *config.Skill).
// This provides a thread-safe way to iterate over session skills from other packages.
func (g *TaskGraph) ForEachSessionSkill(fn func(id string, v any)) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	for id, v := range g.SessionSkills {
		fn(id, v)
	}
}

// GetSessionProvider / SetSessionProvider: same contract as the Role variants above,
// for SessionProviders.
func (g *TaskGraph) GetSessionProvider(id string) (any, bool) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionProviders == nil {
		return nil, false
	}
	v, ok := g.SessionProviders[id]
	return v, ok
}

func (g *TaskGraph) SetSessionProvider(id string, v any) {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	if g.SessionProviders == nil {
		g.SessionProviders = make(map[string]any)
	}
	g.SessionProviders[id] = v
}

func (g *TaskGraph) ListSessionProviderIDs() []string {
	g.sessionMu.Lock()
	defer g.sessionMu.Unlock()
	ids := make([]string, 0, len(g.SessionProviders))
	for id := range g.SessionProviders {
		ids = append(ids, id)
	}
	return ids
}

// ComputeConfidence returns geometric mean of confidence across completed steps.
func (g *TaskGraph) ComputeConfidence() float64 {
	var scores []float64
	for _, s := range g.Steps {
		if s.Confidence != nil && (s.Status == StepOK || s.Status == StepPartial) {
			scores = append(scores, *s.Confidence)
		}
	}
	if len(scores) == 0 {
		return 0
	}
	product := 1.0
	for _, sc := range scores {
		product *= sc
	}
	return math.Pow(product, 1.0/float64(len(scores)))
}

// ReadySteps returns steps whose dependencies are all satisfied.
func (g *TaskGraph) ReadySteps() []*Step {
	var ready []*Step
	for _, s := range g.Steps {
		if s.Status != StepPending {
			continue
		}
		allDone := true
		for _, depID := range s.DependsOn {
			dep, ok := g.Steps[depID]
			if !ok {
				continue
			}
			if dep.Status != StepOK && dep.Status != StepPartial && dep.Status != StepSkipped {
				allDone = false
				break
			}
		}
		if allDone {
			ready = append(ready, s)
		}
	}
	return ready
}

// FallbackFor returns the fallback step for a given step ID, if one exists.
func (g *TaskGraph) FallbackFor(stepID string) *Step {
	for _, s := range g.Steps {
		if s.FallbackFor == stepID {
			return s
		}
	}
	return nil
}

// IsTerminal returns true when all steps have reached a terminal state.
func (g *TaskGraph) IsTerminal() bool {
	for _, s := range g.Steps {
		if !s.IsTerminal() {
			return false
		}
	}
	return true
}

// ToStatusDict produces a serialisable status snapshot.
func (g *TaskGraph) ToStatusDict() map[string]any {
	return g.ToStatusDictView("full")
}

// ToStatusDictView produces a serialisable status snapshot based on the requested view.
// Supported views: "summary", "full" (default).
func (g *TaskGraph) ToStatusDictView(view string) map[string]any {
	if view == "summary" {
		completed := 0
		var activeStepID string
		var lastError string
		for _, s := range g.Steps {
			if s.Status == StepOK || s.Status == StepPartial || s.Status == StepSkipped {
				completed++
			}
			if s.Status == StepRunning || s.Status == StepBlocked {
				activeStepID = s.ID
			}
			if s.LastError != "" {
				lastError = s.LastError
			}
		}

		return map[string]any{
			"task_id":        g.TaskID,
			"status":         g.Status,
			"progress":       fmt.Sprintf("%d/%d", completed, len(g.Steps)),
			"active_step_id": activeStepID,
			"last_error":     lastError,
			"created_at":     g.CreatedAt.Format(time.RFC3339),
		}
	}

	steps := make(map[string]any, len(g.Steps))
	for id, s := range g.Steps {
		steps[id] = map[string]any{
			"id":                s.ID,
			"role_id":           s.RoleID,
			"task":              s.Task,
			"depends_on":        s.DependsOn,
			"read_deps":         s.ReadDeps,
			"additional_skills": s.AdditionalSkills,
			"status":            s.Status,
			"confidence":        s.Confidence,
			"retry_count":       s.RetryCount,
			"result_ref":        s.ResultRef,
			"missing_context":   s.MissingCtx,
			"last_error":        s.LastError,
			"trigger_error":     s.TriggerError,
			"last_worked_on":    s.LastWorkedOn.Format(time.RFC3339),
			"merge_strategy":    s.MergeStrategy,
			"target_key":        s.TargetKey,
			"input_mapping":     s.InputMapping,
			"isolation":         s.Isolation,
			"failure_policy":    s.FailurePolicy,
		}
	}

	decisions := make([]map[string]any, len(g.PendingDecisions))
	for i, d := range g.PendingDecisions {
		// Sanitize context for decisions
		safeCtx := make(map[string]any)
		for k, v := range d.Context {
			if k != "embedding" && k != "embedding_model" {
				safeCtx[k] = v
			}
		}

		decisions[i] = map[string]any{
			"id":      d.ID,
			"step_id": d.StepID,
			"type":    d.Type,
			"context": safeCtx,
			"options": d.Options,
			"created": d.Created.Format(time.RFC3339),
		}
	}

	files := make([]map[string]any, len(g.OutputFiles))
	for i, f := range g.OutputFiles {
		files[i] = map[string]any{
			"step_id":    f.StepID,
			"path":       f.Path,
			"format":     f.Format,
			"summary":    f.Summary,
			"size_bytes": f.SizeBytes,
			"is_primary": f.IsPrimary,
			"created":    f.CreatedAt.Format(time.RFC3339),
		}
	}

	skippedStepsCount := 0
	for _, s := range g.Steps {
		if s.Status == StepSkipped {
			skippedStepsCount++
		}
	}

	// Sanitize DecisionHistory for status reports (hide heavy embeddings)
	sanitizedHistory := make(map[string]map[string]any)
	for id, node := range g.DecisionHistory {
		reasoning := node.Reasoning
		if len(reasoning) > 200 {
			reasoning = reasoning[:200] + "..."
		}
		sanitizedHistory[id] = map[string]any{
			"id":        node.ID,
			"step_id":   node.StepID,
			"turn":      node.Turn,
			"type":      node.Type,
			"reasoning": reasoning,
			"action":    node.Action,
			"outcome":   node.Outcome,
			"causes":    node.Causes,
			"timestamp": node.Timestamp.Format(time.RFC3339),
		}
	}

	// Sanitize ContextTree for status reports
	sanitizedTree := make(map[string]map[string]any)
	for id, node := range g.ContextTree {
		// Sanitize metadata
		safeMeta := make(map[string]any)
		for k, v := range node.Metadata {
			if k != "embedding" && k != "embedding_model" {
				safeMeta[k] = v
			}
		}

		sanitizedTree[id] = map[string]any{
			"id":        node.ID,
			"parent_id": node.ParentID,
			"intent":    node.Intent,
			"goal":      node.Goal,
			"status":    node.Status,
			"summary":   node.Summary,
			"step_ids":  node.StepIDs,
			"metadata":  safeMeta,
		}
	}

	// Sanitize GlobalWorkspace: strip heavy system_prompt/user_prompt from
	// each entry unless the corresponding step is in delegation_required status.
	// These prompts (anti-pattern warnings, JIT suggestions, output contracts)
	// can be 5000+ chars each and are only needed for delegation fulfillment.
	sanitizedWorkspace := make(map[string]any, len(g.GlobalWorkspace))
	delegationSteps := make(map[string]bool)
	for _, s := range g.Steps {
		if s.Status == StepBlocked && s.TriggerError == "delegation_required" {
			delegationSteps[s.ID] = true
		}
	}
	anyDelegation := len(delegationSteps) > 0
	for k, v := range g.GlobalWorkspace {
		keepPrompts := false
		if k == "result" {
			keepPrompts = anyDelegation
		} else if delegationSteps[k] {
			keepPrompts = true
		}
		if !keepPrompts {
			if entry, ok := v.(map[string]any); ok {
				stripped := make(map[string]any, len(entry))
				for ek, ev := range entry {
					if ek == "system_prompt" || ek == "user_prompt" {
						continue
					}
					stripped[ek] = ev
				}
				sanitizedWorkspace[k] = stripped
				continue
			}
		}
		sanitizedWorkspace[k] = v
	}

	res := map[string]any{
		"task_id":             g.TaskID,
		"status":              g.Status,
		"overall_confidence":  g.OverallConfidence,
		"skipped_steps_count": skippedStepsCount,
		"steps":               steps,
		"pending_decisions":   decisions,
		"output_files":        files,
		"artifacts":           g.Artifacts,
		"global_workspace":    sanitizedWorkspace,
		"created_at":          g.CreatedAt.Format(time.RFC3339),
		"session_roles":       g.SessionRoles,
		"session_skills":      g.SessionSkills,
		"coordination_edges":  g.CoordinationEdges,
		"decision_history":    sanitizedHistory,
		"context_tree":        sanitizedTree,
		"current_node_id":     g.CurrentNodeID,
	}
	if g.CompletedAt != nil {
		res["completed_at"] = g.CompletedAt.Format(time.RFC3339)
	}
	return res
}

// ─── ID generators ────────────────────────────────────────────────────────

func NewTaskID() string {
	return fmt.Sprintf("task_%s", uuid.New().String()[:8])
}

func NewDecisionID() string {
	return fmt.Sprintf("dec_%s", uuid.New().String()[:6])
}
