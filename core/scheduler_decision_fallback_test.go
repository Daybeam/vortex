package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	pkg_interfaces "github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// fakeExperienceStore is a minimal, fully-stubbed store.IExperienceStore
// implementation for testing handleOutput's FallbackFor pre-block
// activation and SelectSkill/QuerySkillRecommendations UCB1 wiring (both
// re-wired 2026-09-05, per the 09-06 addendum) without needing a real
// ExperienceStore or any persisted data. Only the two Fn fields relevant
// to a given test are set; every other method returns a harmless zero
// value so the fake still fully satisfies the interface.
type fakeExperienceStore struct {
	QuerySkillRecommendationsFn func(ctx context.Context, capability string, minConfidence float64) []string
	SelectSkillFn               func(ctx context.Context, availableSkills []string, capability, roleID, modelID string, epsilon float64) string
}

var _ store.IExperienceStore = (*fakeExperienceStore)(nil)

func (f *fakeExperienceStore) RecordTaskCompletion(ctx context.Context, taskID string, records []store.StepRecord, overallConf float64, decisions []map[string]any, success bool, attributableFailure bool) error {
	return nil
}
func (f *fakeExperienceStore) UpdateRouteWeight(ctx context.Context, roleID, modelID, skill, capability string, score float64, success bool) error {
	return nil
}
func (f *fakeExperienceStore) SelectSkill(ctx context.Context, availableSkills []string, capability, roleID, modelID string, epsilon float64) string {
	if f.SelectSkillFn != nil {
		return f.SelectSkillFn(ctx, availableSkills, capability, roleID, modelID, epsilon)
	}
	if len(availableSkills) > 0 {
		return availableSkills[0]
	}
	return ""
}
func (f *fakeExperienceStore) AddGeneratedSkill(ctx context.Context, skill *store.GeneratedSkill) error {
	return nil
}
func (f *fakeExperienceStore) QueryRoleAdvice(ctx context.Context, roleID string) map[string]any {
	return nil
}
func (f *fakeExperienceStore) RetrieveRelevantExperience(ctx context.Context, query []float32, capability, errorSignal string, tokenBudget int) []*store.ExperienceNode {
	return nil
}
func (f *fakeExperienceStore) QuerySimilarPatterns(ctx context.Context, capabilities []string, limit int) []store.TaskPattern {
	return nil
}
func (f *fakeExperienceStore) QueryRelevantSkills(ctx context.Context, taskType string, limit int) []string {
	return nil
}
func (f *fakeExperienceStore) QuerySkillsBySignal(ctx context.Context, signal string) []string {
	return nil
}
func (f *fakeExperienceStore) GetOrchestrationBrief(ctx context.Context, skillIDs []string) map[string]any {
	return nil
}
func (f *fakeExperienceStore) PersistAll(ctx context.Context) error            { return nil }
func (f *fakeExperienceStore) PruneSkills() []string                           { return nil }
func (f *fakeExperienceStore) RootSkillForCapability(capability string) string { return "" }
func (f *fakeExperienceStore) QuerySkillRecommendations(ctx context.Context, capability string, minConfidence float64) []string {
	if f.QuerySkillRecommendationsFn != nil {
		return f.QuerySkillRecommendationsFn(ctx, capability, minConfidence)
	}
	return nil
}
func (f *fakeExperienceStore) QueryDecisionAdvice(ctx context.Context, decisionType, roleID string) *store.DecisionOutcome {
	return nil
}
func (f *fakeExperienceStore) RecordEnvironmentIssue(ctx context.Context, cmd string, message string) error {
	return nil
}
func (f *fakeExperienceStore) GetEnvironmentAlerts(ctx context.Context) map[string]any { return nil }
func (f *fakeExperienceStore) GetGeneratedSkill(id string) (*store.GeneratedSkill, bool) {
	return nil, false
}
func (f *fakeExperienceStore) GetGeneratedSkillsSnapshot() map[string]*store.GeneratedSkill {
	return nil
}
func (f *fakeExperienceStore) UpdateGeneratedSkillVerified(id string, verified bool) {}
func (f *fakeExperienceStore) IncrementGeneratedSkillUsage(id string)                {}
func (f *fakeExperienceStore) QueryRoleAffinity(ctx context.Context, roleID, taskType string) float64 {
	return 0
}
func (f *fakeExperienceStore) GetStatePotential(stateHash string) float64 {
	return 1.0
}
func (f *fakeExperienceStore) QueryRelevantAntiPatterns(intent string, limit int) []store.AntiPatternPrecedent {
	return nil
}
func (f *fakeExperienceStore) UpsertDecisionPrecedent(ctx context.Context, node *schemas.DecisionNode) error {
	return nil
}
func (f *fakeExperienceStore) QueryDecisionPrecedents(ctx context.Context, intent string, limit int) []*schemas.DecisionNode {
	return nil
}
func (f *fakeExperienceStore) QueryJITCandidates(ctx context.Context, taskText string, minSamples int, minConfidence float64, limit int) []store.TaskPattern {
	return nil
}
func (f *fakeExperienceStore) PruneStaleJITRefs(reg *config.Registry) int { return 0 }
func (f *fakeExperienceStore) Reindex(ctx context.Context, provider pkg_interfaces.Provider, modelID string) error {
	return nil
}
func (f *fakeExperienceStore) GetRoleProfilesSnapshot() map[string]*store.RoleProfile { return nil }
func (f *fakeExperienceStore) GetTaskPatternsSnapshot() map[string]store.TaskPattern  { return nil }
func (f *fakeExperienceStore) GetTelemetrySnapshot(detailed bool) map[string]any      { return nil }
func (f *fakeExperienceStore) GetCompoundCandidates(ctx context.Context, minCount int, minSuccessRate float64) ([]*store.CooccurrenceEntry, error) {
	return nil, nil
}
func (f *fakeExperienceStore) GetCooccurrencePartners(ctx context.Context, toolA string, minCount int) ([]*store.CooccurrenceEntry, error) {
	return nil, nil
}
func (f *fakeExperienceStore) PruneByMetabolicROI() []string { return nil }
func (f *fakeExperienceStore) WaitAsyncSaves()               {}

// newTestEngineForHandleOutput builds a minimal, bare &DirectedEngine{} for
// testing handleOutput in isolation, following the exact same construction
// pattern already established and verified working by
// TestExecuteStep_BudgetExceeded_DoesNotDeadlock (core/budget_guard_deadlock_test.go).
func newTestEngineForHandleOutput(t *testing.T, exp store.IExperienceStore) *DirectedEngine {
	t.Helper()
	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	return &DirectedEngine{
		logger:     logger,
		outputBase: t.TempDir(),
		doneChans:  make(map[string]chan struct{}),
		expStore:   exp,
		registry: &config.Registry{
			System: config.SystemSettings{
				ConfidenceThreshold: 0.7,
			},
		},
	}
}

func capabilityRequiredResult(caps []string) *SpawnResult {
	return &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:               schemas.StatusCapabilityRequired,
			Confidence:           0.9,
			RequiredCapabilities: caps,
		},
	}
}

// ─── FallbackFor pre-block activation (S3.2 of the 09-06 addendum) ─────────

func TestHandleOutput_FallbackFor_ActivatesBeforeDecisionBlock(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_fallback_active_test",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	mainStep := &schemas.Step{ID: "s1", Status: schemas.StepPending}
	fallbackStep := &schemas.Step{ID: "s2", Status: schemas.StepPending, FallbackFor: "s1"}
	graph.Steps[mainStep.ID] = mainStep
	graph.Steps[fallbackStep.ID] = fallbackStep

	engine.handleOutput(graph, mainStep, capabilityRequiredResult([]string{"web_search"}))

	if mainStep.Status != schemas.StepFailed {
		t.Fatalf("expected main step Status == StepFailed (failed-with-fallback), got %v", mainStep.Status)
	}
	if fallbackStep.Status != schemas.StepPending {
		t.Fatalf("expected fallback step to remain StepPending so the scheduler picks it up, got %v", fallbackStep.Status)
	}
	if len(graph.PendingDecisions) != 0 {
		t.Fatalf("expected zero pending decisions when a fallback exists and is activated, got %d", len(graph.PendingDecisions))
	}

	engine.logger.Close()
	events, err := engine.logger.ReadTaskLogs(graph.TaskID)
	if err != nil {
		t.Fatalf("ReadTaskLogs: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev["event"] == string(EventFallbackActivated) {
			found = true
			detail, _ := ev["detail"].(map[string]any)
			if detail["fallback_step"] != "s2" {
				t.Fatalf("expected fallback_step=s2 in EventFallbackActivated detail, got %v", detail["fallback_step"])
			}
			break
		}
	}
	if !found {
		t.Fatal("expected EventFallbackActivated to be logged")
	}
}

func TestHandleOutput_FallbackFor_AbsentFallsThroughToDecision(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_no_fallback_test",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepPending}
	graph.Steps[step.ID] = step

	result := &SpawnResult{
		Output: schemas.SubagentOutput{Status: schemas.StatusPartial, Confidence: 0.7},
	}
	engine.handleOutput(graph, step, result)

	if step.Status != schemas.StepBlocked {
		t.Fatalf("expected step.Status == StepBlocked when no fallback exists, got %v", step.Status)
	}
	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected exactly 1 pending decision, got %d", len(graph.PendingDecisions))
	}
}

func TestHandleOutput_FallbackFor_NotPendingFallsThroughToDecision(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_fallback_nonpending_test",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	mainStep := &schemas.Step{ID: "s1", Status: schemas.StepPending}
	// Fallback exists but has already been consumed/skipped -- must not be
	// treated as available, and the main step should fall through to the
	// normal decision-block path instead.
	fallbackStep := &schemas.Step{ID: "s2", Status: schemas.StepSkipped, FallbackFor: "s1"}
	graph.Steps[mainStep.ID] = mainStep
	graph.Steps[fallbackStep.ID] = fallbackStep

	result := &SpawnResult{
		Output: schemas.SubagentOutput{Status: schemas.StatusPartial, Confidence: 0.7},
	}
	engine.handleOutput(graph, mainStep, result)

	if mainStep.Status != schemas.StepBlocked {
		t.Fatalf("expected StepBlocked (fallback exists but not Pending, so falls through), got %v", mainStep.Status)
	}
	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected exactly 1 pending decision, got %d", len(graph.PendingDecisions))
	}
}

// ─── SelectSkill UCB1 wiring into retry_with_skill options (S3.2) ──────────

func TestHandleOutput_SelectSkill_UCB1WiringBuildsRankedOption(t *testing.T) {
	var gotRecommendCap string
	var gotSelectSkills []string
	exp := &fakeExperienceStore{
		QuerySkillRecommendationsFn: func(ctx context.Context, capability string, minConfidence float64) []string {
			gotRecommendCap = capability
			return []string{"skill_a", "skill_b"}
		},
		SelectSkillFn: func(ctx context.Context, availableSkills []string, capability, roleID, modelID string, epsilon float64) string {
			gotSelectSkills = availableSkills
			return "skill_b" // pretend UCB1 picked skill_b
		},
	}
	engine := newTestEngineForHandleOutput(t, exp)

	graph := &schemas.TaskGraph{
		TaskID:           "task_selectskill_test",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepPending, RoleID: "role_x"}
	graph.Steps[step.ID] = step

	engine.handleOutput(graph, step, capabilityRequiredResult([]string{"web_search"}))

	if gotRecommendCap != "web_search" {
		t.Fatalf("expected QuerySkillRecommendations called with capability=web_search, got %q", gotRecommendCap)
	}
	if len(gotSelectSkills) != 2 || gotSelectSkills[0] != "skill_a" || gotSelectSkills[1] != "skill_b" {
		t.Fatalf("expected SelectSkill called with the recommended candidates [skill_a skill_b], got %v", gotSelectSkills)
	}
	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected exactly 1 pending decision, got %d", len(graph.PendingDecisions))
	}

	options := graph.PendingDecisions[0].Options
	wantRanked := "retry_with_skill:skill_b"
	wantRaw := "retry_with_skill:web_search"
	foundRanked, foundRaw := false, false
	for _, o := range options {
		if o == wantRanked {
			foundRanked = true
		}
		if o == wantRaw {
			foundRaw = true
		}
	}
	if !foundRanked {
		t.Fatalf("expected UCB1-selected option %q among options %v", wantRanked, options)
	}
	if !foundRaw {
		t.Fatalf("expected raw-capability cold-start fallback option %q among options %v", wantRaw, options)
	}
}

func TestHandleOutput_SelectSkill_NilExpStoreFallsBackToRawCapability(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_selectskill_nil_test",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepPending, RoleID: "role_x"}
	graph.Steps[step.ID] = step

	engine.handleOutput(graph, step, capabilityRequiredResult([]string{"web_search"}))

	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected exactly 1 pending decision, got %d", len(graph.PendingDecisions))
	}
	options := graph.PendingDecisions[0].Options
	want := "retry_with_skill:web_search"
	found := false
	for _, o := range options {
		if o == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected fallback option %q among options %v when expStore is nil", want, options)
	}
}

func TestHandleOutput_SelectSkill_NoRecommendationsColdStart(t *testing.T) {
	selectSkillCalled := false
	exp := &fakeExperienceStore{
		QuerySkillRecommendationsFn: func(ctx context.Context, capability string, minConfidence float64) []string {
			return nil // cold start: nothing learned yet
		},
		SelectSkillFn: func(ctx context.Context, availableSkills []string, capability, roleID, modelID string, epsilon float64) string {
			selectSkillCalled = true
			return ""
		},
	}
	engine := newTestEngineForHandleOutput(t, exp)

	graph := &schemas.TaskGraph{
		TaskID:           "task_selectskill_coldstart_test",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepPending, RoleID: "role_x"}
	graph.Steps[step.ID] = step

	engine.handleOutput(graph, step, capabilityRequiredResult([]string{"web_search"}))

	if selectSkillCalled {
		t.Fatal("SelectSkill should not be called when QuerySkillRecommendations returns no candidates (len(recommended)==0 guard)")
	}
	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected exactly 1 pending decision, got %d", len(graph.PendingDecisions))
	}
	options := graph.PendingDecisions[0].Options
	want := "retry_with_skill:web_search"
	found := false
	for _, o := range options {
		if o == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected raw-capability fallback option %q among options %v", want, options)
	}
}
