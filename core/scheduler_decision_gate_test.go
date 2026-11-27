package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// ── containsWord ──────────────────────────────────────────────────────────

func TestContainsWord_ExactMatch(t *testing.T) {
	if !containsWord("skip", "skip") {
		t.Error("exact match should return true")
	}
}

func TestContainsWord_WordInSentence(t *testing.T) {
	if !containsWord("i recommend skip because", "skip") {
		t.Error("whole word in sentence should match")
	}
}

func TestContainsWord_SubstringNotMatch(t *testing.T) {
	// "skip" inside "skipping" must NOT match — this is the audit M2 bug.
	if containsWord("skipping the step", "skip") {
		t.Error("substring inside larger word should not match")
	}
}

func TestContainsWord_StartBoundary(t *testing.T) {
	if !containsWord("skip this step", "skip") {
		t.Error("word at start with space after should match")
	}
}

func TestContainsWord_EndBoundary(t *testing.T) {
	if !containsWord("please skip", "skip") {
		t.Error("word at end with space before should match")
	}
}

func TestContainsWord_EmptyWord(t *testing.T) {
	if containsWord("anything", "") {
		t.Error("empty word should not match")
	}
}

func TestContainsWord_MultipleOccurrences(t *testing.T) {
	// First occurrence is substring, second is whole word.
	if !containsWord("skipping is bad, skip instead", "skip") {
		t.Error("should find the whole-word occurrence after the substring")
	}
}

// ── isWordChar ────────────────────────────────────────────────────────────

func TestIsWordChar_Letters(t *testing.T) {
	// isWordChar only handles lowercase — callers ToLower before invoking
	// containsWord, so uppercase never reaches this function.
	for c := byte('a'); c <= 'z'; c++ {
		if !isWordChar(c) {
			t.Errorf("lowercase letter %q should be a word char", c)
		}
	}
	for c := byte('A'); c <= 'Z'; c++ {
		if isWordChar(c) {
			t.Errorf("uppercase letter %q should NOT be a word char (callers ToLower first)", c)
		}
	}
}

func TestIsWordChar_Digits(t *testing.T) {
	for c := byte('0'); c <= '9'; c++ {
		if !isWordChar(c) {
			t.Errorf("digit %q should be a word char", c)
		}
	}
}

func TestIsWordChar_Underscore(t *testing.T) {
	if !isWordChar('_') {
		t.Error("underscore should be a word char")
	}
}

func TestIsWordChar_NonWordChars(t *testing.T) {
	nonWord := []byte{' ', '.', ',', '!', '-', '/', ':', ';', '\n', '\t'}
	for _, c := range nonWord {
		if isWordChar(c) {
			t.Errorf("%q should NOT be a word char", c)
		}
	}
}

// ── PublicTierAllowsChoice ────────────────────────────────────────────────

func TestPublicTierAllowsChoice_Allowed(t *testing.T) {
	allowed := []string{"skip", "abort", "retry", "refine_and_retry", "fulfill",
		"resume_more_turns", "replan", "create_role"}
	for _, opt := range allowed {
		if !PublicTierAllowsChoice(opt) {
			t.Errorf("public tier should allow %q", opt)
		}
	}
}

func TestPublicTierAllowsChoice_Denied(t *testing.T) {
	denied := []string{"escalate_model", "rewrite_dag", "retry_with_skill:foo",
		"retry_with_context", "confirm_abort", "continue", ""}
	for _, opt := range denied {
		if PublicTierAllowsChoice(opt) {
			t.Errorf("public tier should NOT allow %q", opt)
		}
	}
}

// ── filterPublicOptions ───────────────────────────────────────────────────

func TestFilterPublicOptions_MixedList(t *testing.T) {
	input := []string{"skip", "escalate_model", "abort", "retry_with_context"}
	got := filterPublicOptions(input)
	want := []string{"skip", "abort"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want[%d]=%q", i, got[i], i, want[i])
		}
	}
}

func TestFilterPublicOptions_AllAdminOnly(t *testing.T) {
	// When every option is admin-only, the original list is kept so the
	// decision stays resolvable (an empty option set would dead-end).
	input := []string{"escalate_model", "rewrite_dag"}
	got := filterPublicOptions(input)
	if len(got) != 2 {
		t.Fatalf("all-admin-only should keep original list, got %v", got)
	}
}

func TestFilterPublicOptions_AllPublic(t *testing.T) {
	input := []string{"skip", "abort", "retry"}
	got := filterPublicOptions(input)
	if len(got) != 3 {
		t.Fatalf("all-public should keep all, got %v", got)
	}
}

// ── filterSkipOptions ─────────────────────────────────────────────────────

func TestFilterSkipOptions_NormalCase(t *testing.T) {
	options := []string{"skip", "retry", "abort"}
	got := filterSkipOptions(0, 3, nil, options)
	if len(got) != 3 {
		t.Fatalf("normal case should keep all options, got %v", got)
	}
}

func TestFilterSkipOptions_MaxSkipsReached(t *testing.T) {
	options := []string{"skip", "retry", "abort"}
	got := filterSkipOptions(3, 3, nil, options)
	// "skip" should be removed, "abort" already present.
	want := []string{"retry", "abort"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want[%d]=%q", i, got[i], i, want[i])
		}
	}
}

func TestFilterSkipOptions_CapabilityRequired(t *testing.T) {
	options := []string{"skip", "retry"}
	ctx := map[string]any{"root_cause": "capability_required"}
	got := filterSkipOptions(0, 3, ctx, options)
	// "skip" removed, "abort" auto-added.
	foundAbort := false
	for _, o := range got {
		if o == "abort" {
			foundAbort = true
		}
		if o == "skip" {
			t.Error("skip should have been removed for capability_required")
		}
	}
	if !foundAbort {
		t.Error("abort should have been auto-added when skip is removed")
	}
}

func TestFilterSkipOptions_ContextDeficit(t *testing.T) {
	options := []string{"skip", "retry"}
	ctx := map[string]any{"root_cause": "context_deficit"}
	got := filterSkipOptions(0, 3, ctx, options)
	for _, o := range got {
		if o == "skip" {
			t.Error("skip should have been removed for context_deficit")
		}
	}
}

func TestFilterSkipOptions_AbortAlreadyPresent(t *testing.T) {
	options := []string{"skip", "abort"}
	ctx := map[string]any{"root_cause": "capability_required"}
	got := filterSkipOptions(0, 3, ctx, options)
	// "skip" removed, "abort" already there — should not be duplicated.
	abortCount := 0
	for _, o := range got {
		if o == "abort" {
			abortCount++
		}
	}
	if abortCount != 1 {
		t.Errorf("abort should appear exactly once, got %d", abortCount)
	}
}

// ── filterOptionsForTier ──────────────────────────────────────────────────

func TestFilterOptionsForTier_AdminNoPublicFilter(t *testing.T) {
	options := []string{"skip", "escalate_model", "abort"}
	got := filterOptionsForTier(0, 3, nil, options, true)
	// Admin: no public filtering, no skip cascade — all kept.
	if len(got) != 3 {
		t.Fatalf("admin should keep all options, got %v", got)
	}
}

func TestFilterOptionsForTier_NonAdminFiltersPublic(t *testing.T) {
	options := []string{"skip", "escalate_model", "abort"}
	got := filterOptionsForTier(0, 3, nil, options, false)
	// Non-admin: escalate_model removed by public filter.
	want := []string{"skip", "abort"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFilterOptionsForTier_SkipCascadeAppliesToAdmin(t *testing.T) {
	options := []string{"skip", "retry"}
	got := filterOptionsForTier(3, 3, nil, options, true)
	// Skip cascade applies regardless of tier.
	for _, o := range got {
		if o == "skip" {
			t.Error("skip should be removed when maxSkips reached, even for admin")
		}
	}
}

// ── defaultNonInteractiveChoice ───────────────────────────────────────────

func TestDefaultNonInteractiveChoice_StepFailedPrefersSkip(t *testing.T) {
	options := []string{"skip", "retry", "abort"}
	choice, ok := defaultNonInteractiveChoice(schemas.DecisionStepFailed, options)
	if !ok || choice != "skip" {
		t.Errorf("step_failed should prefer skip, got %q ok=%v", choice, ok)
	}
}

func TestDefaultNonInteractiveChoice_StepFailedFallbackChain(t *testing.T) {
	// skip not available → retry → refine_and_retry → abort
	options := []string{"refine_and_retry", "abort"}
	choice, ok := defaultNonInteractiveChoice(schemas.DecisionStepFailed, options)
	if !ok || choice != "refine_and_retry" {
		t.Errorf("step_failed without skip should prefer refine_and_retry, got %q ok=%v", choice, ok)
	}
}

func TestDefaultNonInteractiveChoice_StepFailedOnlyAbort(t *testing.T) {
	options := []string{"abort"}
	choice, ok := defaultNonInteractiveChoice(schemas.DecisionStepFailed, options)
	if !ok || choice != "abort" {
		t.Errorf("step_failed with only abort should pick abort, got %q ok=%v", choice, ok)
	}
}

func TestDefaultNonInteractiveChoice_MaxTurnsPrefersResume(t *testing.T) {
	options := []string{"resume_more_turns", "skip", "abort"}
	choice, ok := defaultNonInteractiveChoice(schemas.DecisionMaxTurnsExhausted, options)
	if !ok || choice != "resume_more_turns" {
		t.Errorf("max_turns should prefer resume_more_turns, got %q ok=%v", choice, ok)
	}
}

func TestDefaultNonInteractiveChoice_AutonomousAbortPrefersContinue(t *testing.T) {
	options := []string{"confirm_abort", "continue"}
	choice, ok := defaultNonInteractiveChoice(schemas.DecisionAutonomousAbortRequested, options)
	if !ok || choice != "continue" {
		t.Errorf("autonomous_abort should prefer continue, got %q ok=%v", choice, ok)
	}
}

func TestDefaultNonInteractiveChoice_HumanApprovalPrefersAbort(t *testing.T) {
	options := []string{"abort", "skip"}
	choice, ok := defaultNonInteractiveChoice(schemas.DecisionHumanApprovalRequired, options)
	if !ok || choice != "abort" {
		t.Errorf("human_approval should prefer abort, got %q ok=%v", choice, ok)
	}
}

func TestDefaultNonInteractiveChoice_UnknownTypeReturnsFalse(t *testing.T) {
	options := []string{"skip", "abort"}
	_, ok := defaultNonInteractiveChoice(schemas.DecisionType("unknown_type"), options)
	if ok {
		t.Error("unknown decision type should return false")
	}
}

func TestDefaultNonInteractiveChoice_NoMatchingOption(t *testing.T) {
	// None of the preferred options are available.
	options := []string{"rewrite_dag"}
	_, ok := defaultNonInteractiveChoice(schemas.DecisionStepFailed, options)
	if ok {
		t.Error("should return false when no preferred option is available")
	}
}

func TestDefaultNonInteractiveChoice_DelegationRequired(t *testing.T) {
	options := []string{"fulfill", "skip", "abort"}
	choice, ok := defaultNonInteractiveChoice(schemas.DecisionDelegationRequired, options)
	if !ok || choice != "skip" {
		t.Errorf("delegation should prefer skip, got %q ok=%v", choice, ok)
	}
}

func TestDefaultNonInteractiveChoice_BudgetExhausted(t *testing.T) {
	options := []string{"skip", "abort"}
	choice, ok := defaultNonInteractiveChoice(schemas.DecisionBudgetExhausted, options)
	if !ok || choice != "skip" {
		t.Errorf("budget_exhausted should prefer skip, got %q ok=%v", choice, ok)
	}
}
