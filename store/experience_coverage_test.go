package store

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestFileExperienceBackend_Save(t *testing.T) {
	b := &FileExperienceBackend{dir: t.TempDir()}
	data := map[string]any{"key1": map[string]string{"v": "1"}}
	if err := b.Save(context.Background(), data); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
}

func TestFileExperienceBackend_Load(t *testing.T) {
	b := &FileExperienceBackend{dir: t.TempDir()}
	data, err := b.Load(context.Background())
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty map, got %d items", len(data))
	}
}

func TestFileExperienceBackend_NoOpMethods(t *testing.T) {
	b := &FileExperienceBackend{dir: t.TempDir()}
	ctx := context.Background()
	if err := b.SaveDecisionOutcome(ctx, &DecisionOutcome{}); err != nil {
		t.Errorf("SaveDecisionOutcome: %v", err)
	}
	if err := b.SaveTaskPattern(ctx, &TaskPattern{}); err != nil {
		t.Errorf("SaveTaskPattern: %v", err)
	}
	if err := b.SaveRoleProfile(ctx, &RoleProfile{}); err != nil {
		t.Errorf("SaveRoleProfile: %v", err)
	}
	if err := b.SaveRouteWeight(ctx, &RouteWeight{}); err != nil {
		t.Errorf("SaveRouteWeight: %v", err)
	}
	if err := b.SaveGeneratedSkill(ctx, &GeneratedSkill{}); err != nil {
		t.Errorf("SaveGeneratedSkill: %v", err)
	}
	if err := b.SaveSkillAffinity(ctx, &SkillAffinity{}); err != nil {
		t.Errorf("SaveSkillAffinity: %v", err)
	}
	if err := b.SaveEnvironmentIssue(ctx, &EnvironmentIssue{}); err != nil {
		t.Errorf("SaveEnvironmentIssue: %v", err)
	}
	if err := b.SaveRoleAffinity(ctx, "r1", "coding", 0.8); err != nil {
		t.Errorf("SaveRoleAffinity: %v", err)
	}
	if err := b.SaveDecisionPrecedent(ctx, &schemas.DecisionNode{}); err != nil {
		t.Errorf("SaveDecisionPrecedent: %v", err)
	}
	if err := b.SaveStatePotential(ctx, &StatePotential{}); err != nil {
		t.Errorf("SaveStatePotential: %v", err)
	}
	if err := b.SaveCooccurrence(ctx, &CooccurrenceEntry{}); err != nil {
		t.Errorf("SaveCooccurrence: %v", err)
	}
}

func TestExperienceStore_GetCooccurrencePartners_NilBackend(t *testing.T) {
	es := &ExperienceStore{backend: nil}
	result, err := es.GetCooccurrencePartners(context.Background(), "toolA", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil result with nil backend")
	}
}

func TestExperienceStore_GetGeneratedSkillsSnapshot(t *testing.T) {
	es := &ExperienceStore{
		GeneratedSkills: map[string]*GeneratedSkill{
			"gs1": {ID: "gs1", Description: "skill1"},
			"gs2": {ID: "gs2", Description: "skill2"},
		},
	}
	snapshot := es.GetGeneratedSkillsSnapshot()
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(snapshot))
	}
	if snapshot["gs1"] == nil || snapshot["gs2"] == nil {
		t.Error("expected both skills in snapshot")
	}
}

func TestExperienceStore_LockUnlock(t *testing.T) {
	es := &ExperienceStore{}
	es.Lock()
	es.Unlock()
}

func TestExperienceStore_UpdateGeneratedSkillVerified(t *testing.T) {
	es := &ExperienceStore{
		GeneratedSkills: map[string]*GeneratedSkill{
			"gs1": {ID: "gs1", IsVerified: false},
		},
	}
	es.UpdateGeneratedSkillVerified("gs1", true)
	if !es.GeneratedSkills["gs1"].IsVerified {
		t.Error("expected IsVerified to be true")
	}
	es.UpdateGeneratedSkillVerified("nonexistent", true)
}

func TestExperienceStore_QueryRoleAdvice(t *testing.T) {
	es := &ExperienceStore{
		RoleProfiles: map[string]*RoleProfile{
			"r1": {RoleID: "r1", HighConfidenceSkillCombos: [][]string{{"s1", "s2"}}},
		},
	}
	advice := es.QueryRoleAdvice(context.Background(), "r1")
	if advice == nil {
		t.Fatal("expected non-nil advice")
	}
	combos, ok := advice["recommended_skill_combos"]
	if !ok {
		t.Error("expected recommended_skill_combos key")
	}
	if combos == nil {
		t.Error("expected non-nil combos")
	}
	advice = es.QueryRoleAdvice(context.Background(), "nonexistent")
	if len(advice) != 0 {
		t.Errorf("expected empty advice for nonexistent role, got %v", advice)
	}
}

func TestExperienceStore_GetTelemetrySnapshot(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns: map[string]TaskPattern{
			"p1": {ID: "p1", TaskID: "t1", SourceText: "secret"},
		},
		RoleProfiles: map[string]*RoleProfile{
			"r1": {RoleID: "r1"},
		},
		GeneratedSkills: map[string]*GeneratedSkill{
			"gs1": {ID: "gs1", SourceText: "secret"},
		},
	}
	snapshot := es.GetTelemetrySnapshot(false)
	if snapshot == nil {
		t.Fatal("expected non-nil snapshot")
	}
	patterns, ok := snapshot["task_patterns"].(map[string]TaskPattern)
	if !ok {
		t.Fatal("expected task_patterns in snapshot")
	}
	if patterns["p1"].TaskID != "" {
		t.Error("expected TaskID to be sanitized")
	}
	if patterns["p1"].SourceText != "" {
		t.Error("expected SourceText to be sanitized in non-detailed mode")
	}
	snapshot = es.GetTelemetrySnapshot(true)
	patterns = snapshot["task_patterns"].(map[string]TaskPattern)
	if patterns["p1"].SourceText != "secret" {
		t.Error("expected SourceText to be preserved in detailed mode")
	}
}

func TestExperienceStore_GetRoleProfilesSnapshot(t *testing.T) {
	es := &ExperienceStore{
		RoleProfiles: map[string]*RoleProfile{
			"r1": {RoleID: "r1", TotalRuns: 10},
		},
	}
	snapshot := es.GetRoleProfilesSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(snapshot))
	}
	if snapshot["r1"].TotalRuns != 10 {
		t.Errorf("TotalRuns = %d, want 10", snapshot["r1"].TotalRuns)
	}
}

func TestExperienceStore_GetTaskPatternsSnapshot(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns: map[string]TaskPattern{
			"p1": {ID: "p1", TaskType: "coding"},
		},
	}
	snapshot := es.GetTaskPatternsSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(snapshot))
	}
	if snapshot["p1"].TaskType != "coding" {
		t.Errorf("TaskType = %q, want coding", snapshot["p1"].TaskType)
	}
}

func TestExperienceStore_SetLifecycleContext(t *testing.T) {
	es := &ExperienceStore{}
	ctx := context.Background()
	es.SetLifecycleContext(ctx)
}

func TestExperienceStore_GetOrchestrationBrief(t *testing.T) {
	dir := t.TempDir()
	es, err := NewExperienceStore(dir, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore failed: %v", err)
	}
	brief := es.GetOrchestrationBrief(context.Background(), []string{"skill1"})
	if brief == nil {
		t.Fatal("expected non-nil brief")
	}
	if _, ok := brief["similar_patterns"]; !ok {
		t.Error("expected similar_patterns key")
	}
}

func TestExperienceStore_QueryRoleAffinity(t *testing.T) {
	es := &ExperienceStore{
		RoleAffinities: map[string]map[string]float64{
			"r1": {"coding": 0.8, "testing": 0.5},
		},
	}
	score := es.QueryRoleAffinity(context.Background(), "r1", "coding")
	if score != 0.8 {
		t.Errorf("coding score = %f, want 0.8", score)
	}
	score = es.QueryRoleAffinity(context.Background(), "r1", "nonexistent")
	if score != 0.5 {
		t.Errorf("expected 0.5 for nonexistent taskType, got %f", score)
	}
	score = es.QueryRoleAffinity(context.Background(), "nonexistent", "coding")
	if score != 0.5 {
		t.Errorf("expected 0.5 for nonexistent role, got %f", score)
	}
}

func TestExperienceStore_GetEnvironmentAlerts(t *testing.T) {
	es := &ExperienceStore{
		EnvironmentIssues: map[string]*EnvironmentIssue{
			"e1": {Command: "npm install", Message: "Missing dependency", IsResolved: false},
		},
	}
	alerts := es.GetEnvironmentAlerts(context.Background())
	if alerts == nil {
		t.Fatal("expected non-nil alerts")
	}
}

func TestExperienceStore_QueryDecisionPrecedents(t *testing.T) {
	es := &ExperienceStore{
		DecisionPrecedents: map[string]*schemas.DecisionNode{
			"d1": {ID: "d1", Type: "routing", Reasoning: "chose path A"},
		},
	}
	results := es.QueryDecisionPrecedents(context.Background(), "routing", 10)
	if results != nil {
		t.Errorf("expected nil without embedding client, got %d results", len(results))
	}
	results = es.QueryDecisionPrecedents(context.Background(), "", 10)
	if results != nil {
		t.Errorf("expected nil for empty intent, got %d results", len(results))
	}
}
