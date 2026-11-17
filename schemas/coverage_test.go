package schemas

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestApproveSkillMounts(t *testing.T) {
	g := &TaskGraph{}
	g.ApproveSkillMounts([]string{"skill_1", "skill_2"}, "user1", "web")
	if !g.IsSkillMountApproved("skill_1") {
		t.Error("expected skill_1 to be approved")
	}
	if !g.IsSkillMountApproved("skill_2") {
		t.Error("expected skill_2 to be approved")
	}
	if g.IsSkillMountApproved("skill_3") {
		t.Error("expected skill_3 to NOT be approved")
	}
	rec := g.ApprovedSkillMounts["skill_1"]
	if rec == nil || !rec.Approved {
		t.Error("expected Approved=true for skill_1")
	}
	if rec.ApproverID != "user1" {
		t.Errorf("ApproverID = %q, want %q", rec.ApproverID, "user1")
	}
	if rec.Channel != "web" {
		t.Errorf("Channel = %q, want %q", rec.Channel, "web")
	}
	if rec.Scope != "skill" {
		t.Errorf("Scope = %q, want %q", rec.Scope, "skill")
	}
}

func TestApproveSkillMounts_EmptyAndNil(t *testing.T) {
	var g *TaskGraph
	g.ApproveSkillMounts([]string{"x"}, "u", "c")
	if g.IsSkillMountApproved("x") {
		t.Error("expected false for nil graph")
	}
	g = &TaskGraph{}
	g.ApproveSkillMounts(nil, "u", "c")
	if len(g.ApprovedSkillMounts) != 0 {
		t.Error("expected no entries for nil ids")
	}
	g.ApproveSkillMounts([]string{""}, "u", "c")
	if len(g.ApprovedSkillMounts) != 0 {
		t.Error("expected no entries for empty id")
	}
}

func TestIsSkillMountApproved_EmptyID(t *testing.T) {
	g := &TaskGraph{}
	if g.IsSkillMountApproved("") {
		t.Error("expected false for empty skill ID")
	}
}

func TestIsLowConfidence(t *testing.T) {
	o := &SubagentOutput{Confidence: 0.3}
	if !o.IsLowConfidence() {
		t.Error("expected low confidence for 0.3")
	}
	o.Confidence = 0.6
	if o.IsLowConfidence() {
		t.Error("expected NOT low confidence for 0.6")
	}
	o.Confidence = 0.9
	if o.IsLowConfidence() {
		t.Error("expected NOT low confidence for 0.9")
	}
}

func TestToolConstraintFragment(t *testing.T) {
	out := ToolConstraintFragment([]string{"tool_a", "tool_b"})
	if !strings.Contains(out, "tool_a") {
		t.Error("expected output to contain tool_a")
	}
	if !strings.Contains(out, "tool_b") {
		t.Error("expected output to contain tool_b")
	}
	if !strings.Contains(out, "Tool Usage Restriction") {
		t.Error("expected output to contain header")
	}
	out = ToolConstraintFragment(nil)
	if out != "" {
		t.Errorf("expected empty string for nil, got %q", out)
	}
	out = ToolConstraintFragment([]string{})
	if out != "" {
		t.Errorf("expected empty string for empty slice, got %q", out)
	}
}

func TestSessionRole_GetSet(t *testing.T) {
	g := &TaskGraph{}
	v, ok := g.GetSessionRole("r1")
	if ok {
		t.Error("expected not found for unset role")
	}
	g.SetSessionRole("r1", "role-data")
	v, ok = g.GetSessionRole("r1")
	if !ok {
		t.Error("expected found after set")
	}
	if v != "role-data" {
		t.Errorf("value = %v, want %q", v, "role-data")
	}
}

func TestSessionSkill_GetSet(t *testing.T) {
	g := &TaskGraph{}
	_, ok := g.GetSessionSkill("s1")
	if ok {
		t.Error("expected not found for unset skill")
	}
	g.SetSessionSkill("s1", "skill-data")
	v, ok := g.GetSessionSkill("s1")
	if !ok {
		t.Error("expected found after set")
	}
	if v != "skill-data" {
		t.Errorf("value = %v, want %q", v, "skill-data")
	}
}

func TestForEachSessionSkill(t *testing.T) {
	g := &TaskGraph{}
	g.SetSessionSkill("s1", "data1")
	g.SetSessionSkill("s2", "data2")
	seen := map[string]any{}
	g.ForEachSessionSkill(func(id string, v any) {
		seen[id] = v
	})
	if len(seen) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(seen))
	}
	if seen["s1"] != "data1" {
		t.Errorf("s1 = %v, want data1", seen["s1"])
	}
	if seen["s2"] != "data2" {
		t.Errorf("s2 = %v, want data2", seen["s2"])
	}
}

func TestSessionProvider_GetSet(t *testing.T) {
	g := &TaskGraph{}
	_, ok := g.GetSessionProvider("p1")
	if ok {
		t.Error("expected not found for unset provider")
	}
	g.SetSessionProvider("p1", "provider-data")
	v, ok := g.GetSessionProvider("p1")
	if !ok {
		t.Error("expected found after set")
	}
	if v != "provider-data" {
		t.Errorf("value = %v, want %q", v, "provider-data")
	}
}

func TestListSessionProviderIDs(t *testing.T) {
	g := &TaskGraph{}
	ids := g.ListSessionProviderIDs()
	if len(ids) != 0 {
		t.Fatalf("expected 0 IDs, got %d", len(ids))
	}
	g.SetSessionProvider("p1", "d1")
	g.SetSessionProvider("p2", "d2")
	ids = g.ListSessionProviderIDs()
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs, got %d", len(ids))
	}
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["p1"] || !found["p2"] {
		t.Errorf("expected p1 and p2 in %v", ids)
	}
}

func TestRecordDecision(t *testing.T) {
	g := &TaskGraph{}
	node := &DecisionNode{ID: "dec1", StepID: "s1", Type: "routing"}
	g.RecordDecision("dec1", node)
	if len(g.DecisionHistory) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(g.DecisionHistory))
	}
	if g.DecisionHistory["dec1"] != node {
		t.Error("expected stored node to match")
	}
}

func TestUpdateDecisionOutcome(t *testing.T) {
	g := &TaskGraph{}
	node := &DecisionNode{ID: "dec1", StepID: "s1"}
	g.RecordDecision("dec1", node)
	ok := g.UpdateDecisionOutcome("dec1", "approved")
	if !ok {
		t.Error("expected true for existing decision")
	}
	if g.DecisionHistory["dec1"].Outcome != "approved" {
		t.Errorf("Outcome = %q, want %q", g.DecisionHistory["dec1"].Outcome, "approved")
	}
	ok = g.UpdateDecisionOutcome("nonexistent", "x")
	if ok {
		t.Error("expected false for nonexistent decision")
	}
}

func TestLockUnlockDecisionHistory(t *testing.T) {
	g := &TaskGraph{}
	g.RecordDecision("d1", &DecisionNode{ID: "d1"})
	g.LockDecisionHistory()
	g.UnlockDecisionHistory()
}

func TestComputeConfidence(t *testing.T) {
	g := &TaskGraph{Steps: map[string]*Step{}}
	if c := g.ComputeConfidence(); c != 0 {
		t.Errorf("expected 0 for no steps, got %f", c)
	}
	c1 := 0.8
	c2 := 0.9
	g.Steps["s1"] = &Step{ID: "s1", Status: StepOK, Confidence: &c1}
	g.Steps["s2"] = &Step{ID: "s2", Status: StepPartial, Confidence: &c2}
	g.Steps["s3"] = &Step{ID: "s3", Status: StepPending, Confidence: &c1}
	c := g.ComputeConfidence()
	expected := math.Sqrt(0.8 * 0.9)
	if c < expected-0.001 || c > expected+0.001 {
		t.Errorf("ComputeConfidence = %f, want ~%f", c, expected)
	}
}

func TestFallbackFor(t *testing.T) {
	g := &TaskGraph{Steps: map[string]*Step{}}
	g.Steps["s1"] = &Step{ID: "s1", FallbackFor: "s0"}
	g.Steps["s2"] = &Step{ID: "s2"}
	fb := g.FallbackFor("s0")
	if fb == nil {
		t.Fatal("expected fallback step for s0")
	}
	if fb.ID != "s1" {
		t.Errorf("fallback ID = %q, want %q", fb.ID, "s1")
	}
	fb = g.FallbackFor("nonexistent")
	if fb != nil {
		t.Error("expected nil for nonexistent fallback")
	}
}

func TestToStatusDict(t *testing.T) {
	g := &TaskGraph{
		TaskID:    "t1",
		Status:    GraphRunning,
		Steps:     map[string]*Step{},
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	d := g.ToStatusDict()
	if d["task_id"] != "t1" {
		t.Errorf("task_id = %v, want t1", d["task_id"])
	}
	if d["status"] != "running" {
		t.Errorf("status = %v, want running", d["status"])
	}
}

func TestNewTaskID(t *testing.T) {
	id := NewTaskID()
	if !strings.HasPrefix(id, "task_") {
		t.Errorf("expected task_ prefix, got %q", id)
	}
	id2 := NewTaskID()
	if id == id2 {
		t.Error("expected unique IDs")
	}
}

func TestNewDecisionID(t *testing.T) {
	id := NewDecisionID()
	if !strings.HasPrefix(id, "dec_") {
		t.Errorf("expected dec_ prefix, got %q", id)
	}
	id2 := NewDecisionID()
	if id == id2 {
		t.Error("expected unique IDs")
	}
}

func TestDetermineComplexity(t *testing.T) {
	tests := []struct {
		task      string
		stepCount int
		want      bool
	}{
		{"simple task", 1, false},
		{"edit multiple files", 2, true},
		{"multi-file refactor", 1, true},
		{"update across files", 1, true},
		{"cross-file rename", 1, true},
		{"do A then B then C", 3, true},
		{"refactor the module", 1, true},
		{"migrate to v2", 1, true},
		{"rewrite the handler", 1, true},
		{"restructure the package", 1, true},
		{strings.Repeat("a", 600), 1, true},
		{"simple one-step task", 1, false},
	}
	for _, tt := range tests {
		got := DetermineComplexity(tt.task, tt.stepCount)
		if got != tt.want {
			t.Errorf("DetermineComplexity(%q, %d) = %v, want %v", tt.task, tt.stepCount, got, tt.want)
		}
	}
}
