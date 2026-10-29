package schemas

import (
	"strings"
	"sync"
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
	GraphSuspended          GraphStatus = "suspended" // Control plane: human-initiated pause, resumable
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
	DecisionGoalChangeRequested     DecisionType = "goal_change_requested"       // ADDED (2026-10-29): explicit goal/charter change request — see docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P2
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

// riskTierKeywords holds configurable keyword lists for DetermineRiskTier.
// Defaults preserve the original hardcoded behavior. Override via SetRiskTierKeywords.
// Thread-safe: set once at startup, read concurrently thereafter.
var riskTierKeywords = struct {
	mu       sync.RWMutex
	critical []string
	moderate []string
}{
	critical: []string{"browser", "image", "generate", "scrape", "screenshot", "pollinations", "deploy", "payment"},
	moderate: []string{"patch", "refactor", "build", "test"},
}

// SetRiskTierKeywords overrides the keyword lists used by DetermineRiskTier.
// A nil slice means "leave this tier alone" (keep existing/defaults).
//
// A non-nil EMPTY slice intentionally still means "leave alone" here, because
// this function cannot distinguish "caller wants the tier off" from "caller
// has nothing to say". Use ClearRiskTierKeywords to disable a tier explicitly.
// Config callers go through config.loader, which only invokes this for
// non-nil slices — see config/types.go CriticalRiskKeywords.
func SetRiskTierKeywords(critical, moderate []string) {
	riskTierKeywords.mu.Lock()
	defer riskTierKeywords.mu.Unlock()
	if len(critical) > 0 {
		riskTierKeywords.critical = critical
	}
	if len(moderate) > 0 {
		riskTierKeywords.moderate = moderate
	}
}

// ClearRiskTierKeywords disables keyword matching for the named tiers.
// Pass "critical" and/or "moderate". This is how a deployment turns off a
// default keyword that does not apply to it — e.g. dropping "payment" for an
// eval harness where the word appears in task text but carries no real
// side-effect risk, and where a critical tier needlessly appends an auditor
// step (one extra LLM call per task).
func ClearRiskTierKeywords(tiers ...string) {
	riskTierKeywords.mu.Lock()
	defer riskTierKeywords.mu.Unlock()
	for _, t := range tiers {
		switch t {
		case "critical":
			riskTierKeywords.critical = []string{}
		case "moderate":
			riskTierKeywords.moderate = []string{}
		}
	}
}

// DetermineRiskTier analyzes the task intent and bound MCPs to classify risk.
// Returns both the RiskTier label and a numeric RiskRating (0.0 - 1.0).
func DetermineRiskTier(task string, mcpList []string) (RiskTier, float64) {
	lower := strings.ToLower(task)
	for _, mcp := range mcpList {
		if mcp == "ghost-driver" {
			return RiskTierCritical, 0.95
		}
	}
	riskTierKeywords.mu.RLock()
	critical := riskTierKeywords.critical
	moderate := riskTierKeywords.moderate
	riskTierKeywords.mu.RUnlock()
	for _, kw := range critical {
		if strings.Contains(lower, kw) {
			return RiskTierCritical, 0.9
		}
	}
	for _, kw := range moderate {
		if strings.Contains(lower, kw) {
			return RiskTierModerate, 0.5
		}
	}
	return RiskTierLight, 0.1
}

// DetermineComplexity (ADDED 2026-10-10): heuristic to determine a step task is complex
// enough to warrant an explicit planning phase. Zero-cost keyword matching.
// See docs/plan/STEP_PLAN_MODE_DESIGN.md §9.2 (Option A).
func DetermineComplexity(task string, stepCount int) bool {
	lower := strings.ToLower(task)
	// Multi-file operations → need a plan
	if strings.Contains(lower, "multiple files") || strings.Contains(lower, "multi-file") ||
		strings.Contains(lower, "across files") || strings.Contains(lower, "cross-file") {
		return true
	}
	// Multi-phase operations → need a plan
	if strings.Count(lower, "then") > 1 || strings.Count(lower, "接着") > 1 {
		return true
	}
	// Very long task description → likely complex
	if len(task) > 500 {
		return true
	}
	// Refactor / migrate / rewrite keywords → inherently multi-step
	if strings.Contains(lower, "refactor") || strings.Contains(lower, "migrate") ||
		strings.Contains(lower, "rewrite") || strings.Contains(lower, "restructure") {
		return true
	}
	return false
}
