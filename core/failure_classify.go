package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daybeam/vortex/providers"
)

// FailureClass is a centralized taxonomy of the kinds of step failure this
// engine already recognizes and handles differently. Before this file, the
// classification logic that decides retry/decision/fail-fast behavior was
// scattered across a chain of strings.Contains(err.Error(), "...") checks
// directly inside the scheduler's execution loop (see the 2026-07-24
// transaction-design discussion). This is a faithful, behavior-preserving
// extraction of that logic into one place, so callers (like per-step
// FailurePolicy overrides -- see schemas/task.go's Step.FailurePolicy) have
// a single, independently-testable source of truth instead of re-deriving
// the same string matches themselves.
type FailureClass string

const (
	// FailureClassRateLimit: a 429-style rate limit response. Handled by
	// retrying with backoff (server Retry-After hint preferred when
	// present, see the 2026-07-21 addendum), capped at step.MaxRetries.
	FailureClassRateLimit FailureClass = "rate_limit"

	// FailureClassBadRequest: a 400-style malformed-request response.
	// Handled by immediately skipping the step (not retried -- retrying an
	// identical malformed request would just fail identically).
	FailureClassBadRequest FailureClass = "bad_request"

	// FailureClassAuthPermission: 401/403/PERMISSION_DENIED. Handled by a
	// circuit breaker: fail fast, no retry (a bad/expired key will not
	// become valid by trying again, so retrying only wastes attempts).
	FailureClassAuthPermission FailureClass = "auth_permission"

	// FailureClassMissingDependency: an environment/tooling prerequisite is
	// absent (e.g. a missing binary a JIT/MCP tool needs). Handled via a
	// decision_required block offering skip/abort/retry_with_skill:shell.
	FailureClassMissingDependency FailureClass = "missing_dependency"

	// FailureClassRoleMissing: the requested role_id does not exist (or
	// dynamic generation failed/is disabled). Handled via a
	// decision_required block offering skip/abort (create_role is
	// intentionally never offered here -- see the 2026-07-05/06 F25
	// security decision to remove it from SubmitDecision entirely).
	FailureClassRoleMissing FailureClass = "role_missing"

	// FailureClassTransient is the catch-all for anything not matching a
	// more specific class above -- handled by the existing fixed/server-
	// hint retry schedule via retryWait.
	FailureClassTransient FailureClass = "transient"

	// ── ODFTP Taxonomy (ADDED 2026-08-30) ─────────────────────────────────

	// FailureClassContextDeficit: subagent explicit signal for missing info.
	FailureClassContextDeficit FailureClass = "context_deficit"

	// FailureClassCapabilityRequired: subagent needs a tool/skill not mounted.
	FailureClassCapabilityRequired FailureClass = "capability_required"

	// FailureClassApprovalRequired: a mount-policy `approval` MCP needs human
	// sign-off before a non-admin may use it (G2, 2026-10-22).
	FailureClassApprovalRequired FailureClass = "approval_required"

	// FailureClassGenerativeUncertainty: low confidence output on exploratory task.
	FailureClassGenerativeUncertainty FailureClass = "generative_uncertainty"

	// FailureClassContractViolation: output schema mismatch (e.g. invalid JSON).
	FailureClassContractViolation FailureClass = "contract_violation"

	// FailureClassCostOverrun: cost governance abort — cache inefficiency, budget
	// trajectory, or behavior deviation. ADDED (2026-09-14).
	FailureClassCostOverrun FailureClass = "cost_overrun"

	// FailureClassDeterministicToolRefusal: a tool returned a deterministic or
	// environmental refusal (e.g. an operation the environment forbids), detected
	// via a repeated task-level tool failure count. Distinct from
	// generative_uncertainty: retrying the same call cannot succeed, so the
	// engine must NOT spawn a same-role follow-up (which would just repeat the
	// same failing tool call). ADDED (2026-10-02, E2/E3 fix).
	FailureClassDeterministicToolRefusal FailureClass = "deterministic_tool_refusal"

	// FailureClassResourceNotFound: a 404-style response from an MCP tool.
	// Handled by asking the user to verify the resource identifier.
	// ADDED (2026-10-03) for SERF integration.
	FailureClassResourceNotFound FailureClass = "resource_not_found"
)

// classifyError centralizes the failure-classification logic used by the
// scheduler's retry loop. It is a pure function with no side effects,
// deliberately kept separate from what the scheduler *does* about each
// class -- see the call site in scheduler.go's step-error handling.
func classifyError(err error) FailureClass {
	if err == nil {
		return FailureClassTransient
	}

	var rl *providers.RateLimitError
	if errors.As(err, &rl) {
		return FailureClassRateLimit
	}

	var br *providers.BadRequestError
	if errors.As(err, &br) {
		return FailureClassBadRequest
	}

	// D3 (2026-11-11): a gateway refusing further requests because the run's
	// allowance is spent is a budget abort, not a malformed request. Checked
	// before the BadRequestError branch above would have caught it, because it
	// used to BE a BadRequestError -- which made the DAG path skip the step as
	// "unfixable input" and made the chat path print `bad request: {...}`.
	// cost_overrun is the existing class for exactly this: a governance abort
	// the replay scheduler already treats as unfixable.
	var be *providers.BudgetExhaustedError
	if errors.As(err, &be) {
		return FailureClassCostOverrun
	}

	msg := err.Error()

	// Flattened-form fallback for the same class: by the time LastError is read
	// back (post-SanitizeError, or a re-classification of the stored string at
	// the end of executeStep) the typed error is gone and only this text
	// remains. Without it an exhausted budget would re-classify as transient.
	if strings.Contains(msg, providers.BudgetExhaustedMarker) || strings.Contains(msg, "max_agent_turns_exceeded") {
		return FailureClassCostOverrun
	}

	// FIX (2026-08-07): the typed *providers.RateLimitError check above only
	// catches rate limits at the point they're first raised inside a
	// provider call. By the time a step's error has been flattened to a
	// plain string (e.g. store/experience.go's RecordTaskCompletion reading
	// step.LastError post-SanitizeError, well downstream of the original
	// typed error), that type information is gone -- so without a string
	// fallback here, the single most common transient-failure class would
	// silently fall through to FailureClassTransient for any caller working
	// from a flattened string rather than the live error chain.
	if strings.Contains(msg, "HTTP 429") || strings.Contains(msg, "RESOURCE_EXHAUSTED") || strings.Contains(strings.ToLower(msg), "rate limit") || strings.Contains(strings.ToLower(msg), "rate_limited") {
		return FailureClassRateLimit
	}

	if strings.Contains(msg, "HTTP 403") || strings.Contains(msg, "PERMISSION_DENIED") || strings.Contains(msg, "HTTP 401") {
		return FailureClassAuthPermission
	}

	if strings.Contains(msg, "missing dependency:") {
		return FailureClassMissingDependency
	}

	if strings.Contains(msg, "role \"") && (strings.Contains(msg, "\" not found") || strings.Contains(msg, "generation failed") || strings.Contains(msg, "generation is disabled")) {
		return FailureClassRoleMissing
	}

	return FailureClassTransient
}

// classifyErrorWithSystemOne wraps classifyError with an optional System One
// refinement. When the initial classification is FailureClassTransient (the
// catch-all) and System One is wired with EnableFailureClassify, it asks the
// model for a more specific class. If System One returns a valid class with
// confidence > 0.7, it overrides; otherwise the original transient class is
// kept. On any error or timeout, falls back to the original classification.
func (s *DirectedEngine) classifyErrorWithSystemOne(err error, stepID, roleID, stepTask string) FailureClass {
	initial := classifyError(err)
	if initial != FailureClassTransient {
		return initial
	}
	if s.systemOneProvider == nil || !s.systemOneCfg.EnableFailureClassify {
		return initial
	}
	allClasses := []string{
		"rate_limit", "bad_request", "auth_permission", "missing_dependency",
		"role_missing", "transient", "context_deficit", "capability_required",
		"contract_violation", "cost_overrun", "deterministic_tool_refusal",
	}
	state := fmt.Sprintf("Step %s role %s failed. Error: %s. Task: %s.", stepID, roleID, err.Error(), stepTask)
	questions := map[string]any{
		"q1": map[string]any{
			"type":     "choice",
			"question": "What failure class is this?",
			"options":  allClasses,
		},
	}
	req := providers.CompleteRequest{
		User:        state,
		Model:       s.systemOneCfg.Model,
		MaxTokens:   256,
		Constraints: map[string]any{"questions": questions},
	}
	ctx, cancel := context.WithTimeout(s.lifecycleCtx, 3*time.Second)
	defer cancel()
	resp, rerr := s.systemOneProvider.Complete(ctx, req)
	if rerr != nil {
		return initial
	}
	for _, tc := range resp.ToolCalls {
		if tc.Name != "q1" {
			continue
		}
		choice, _ := tc.Arguments["choice"].(string)
		confidence, _ := tc.Arguments["confidence"].(float64)
		if confidence < 0.7 {
			return initial
		}
		for _, c := range allClasses {
			if c == choice {
				return FailureClass(c)
			}
		}
	}
	return initial
}
