package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/store"
)

// antipattern_injection.go — Module 2 of the Two-Tier Clarification &
// Readiness Design (docs/architecture/TWO_TIER_CLARIFICATION_AND_READINESS_DESIGN.md).
//
// When the Auto-Refine loop rejects a step via verifyExitCriteria, the
// downstream weak model has just failed once. Rather than only feeding it
// the raw auditor rejection text, we query the AntiPatternStore for
// historical precedents matching the step's domain and inject them as a
// protected [PREVIOUS FAILURE ANTI-PATTERN] block. This realizes the
// "error-book self-iteration" guidance signal: the weak model sees both
// the specific rejection AND the curated historical pitfalls, reducing
// its secondary error rate.

// maxAntiPatternInjection caps how many precedents we inject per retry.
// Three is the sweet spot: enough signal, not enough to dilute the prompt.
const maxAntiPatternInjection = 3

// buildAntiPatternGuidance queries the experience store for anti-pattern
// precedents relevant to the failing step and formats them as a protected
// injection block. Returns "" when the store is unavailable or no
// precedents match — callers must handle the empty case by falling back
// to the plain rejection text.
//
// The query intent combines the step's task text and the auditor's
// rejection reason, so we match on both the domain (task) and the
// specific failure mode (reason).
func (s *DirectedEngine) buildAntiPatternGuidance(stepTask, rejectionReason string) string {
	if s.expStore == nil {
		return ""
	}

	// Build a combined intent: the task describes the domain, the rejection
	// reason describes the acute failure. Matching on both maximizes recall
	// of relevant precedents without over-fetching.
	intent := strings.TrimSpace(stepTask)
	if rejectionReason != "" {
		intent += " " + strings.TrimSpace(rejectionReason)
	}
	if intent == "" {
		return ""
	}

	precedents := s.expStore.QueryRelevantAntiPatterns(intent, maxAntiPatternInjection)
	if len(precedents) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("[PREVIOUS FAILURE ANTI-PATTERN]\n")
	b.WriteString("The following historical pitfalls are the most relevant to your current failure.\n")
	b.WriteString("Avoid repeating them. Each entry lists the anti-pattern, the symptom you would see, and the correct pattern to use instead.\n\n")
	for i, p := range precedents {
		b.WriteString(fmt.Sprintf("--- Anti-Pattern %d (confidence %.2f, category %s) ---\n", i+1, p.Confidence, p.Category))
		b.WriteString("ANTI-PATTERN: " + p.AntiPattern + "\n")
		if p.Symptom != "" {
			b.WriteString("SYMPTOM: " + p.Symptom + "\n")
		}
		if p.CorrectPattern != "" {
			b.WriteString("CORRECT PATTERN: " + p.CorrectPattern + "\n")
		}
		if p.TriggerCondition != "" {
			b.WriteString("TRIGGER CONDITION: " + p.TriggerCondition + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("END [PREVIOUS FAILURE ANTI-PATTERN]\n")
	return b.String()
}

// composeRecoveryPrompt builds the full AdditionalPromptContext for an
// Auto-Refine retry. It layers three signals in priority order:
//  1. The specific auditor rejection (always present).
//  2. The historical anti-pattern injection (when available).
//  3. Any prior recovery context already on the spawn request (carried
//     forward so earlier guidance is not lost across multi-refine).
//
// This is the single composition point used by the Auto-Refine loop in
// scheduler_submit.go, keeping the retry-prompt shape consistent and
// testable in isolation.
func composeRecoveryPrompt(existingContext, rejectionReason, antiPatternGuidance string) string {
	var b strings.Builder
	b.WriteString("Your previous attempt was rejected by the exit-criteria audit.\n")
	b.WriteString("REJECTION REASON: " + rejectionReason + "\n")
	b.WriteString("Please address this specific failure and complete all required steps. ")
	b.WriteString("Do NOT invent data or report success for steps you did not actually execute.")

	if antiPatternGuidance != "" {
		b.WriteString("\n\n")
		b.WriteString(antiPatternGuidance)
	}

	composed := b.String()

	// Preserve any prior context (e.g. from a previous refine iteration or
	// an ODFTP recovery seed) so we don't drop earlier guidance mid-loop.
	if existingContext != "" {
		composed = existingContext + "\n\n" + composed
	}
	return composed
}

// Compile-time check that SanitizeError is reachable from store — keeps
// the import meaningful even if the helper above evolves.
var _ = store.SanitizeError
