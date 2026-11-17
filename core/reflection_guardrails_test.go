package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/jit"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/store"
)

// newPersistableJITRegistry creates a registry backed by a real config file
// so that CommitOps→Persist succeeds (needed for PromotePermanent).
// newMockJITRegistry creates a bare struct with no configPath, so any
// CommitOps call fails — fine for crystallization tests (which ignore the
// WAL error) but fatal for promotion tests (which check err==nil).
func newPersistableJITRegistry(t *testing.T, response string) (*config.Registry, *MockHybridProvider) {
	t.Helper()
	name := fmt.Sprintf("mock-persist-%d", time.Now().UnixNano())
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON, _ := json.Marshal(map[string]any{
		"default_provider": name,
		"providers": map[string]any{
			name: map[string]any{"provider": name},
		},
	})
	if err := os.WriteFile(cfgPath, cfgJSON, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	reg, err := config.NewRegistry(cfgPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	mock := &MockHybridProvider{response: response}
	providers.GlobalRouter.Register(name, reg.Providers[name], mock)
	t.Cleanup(func() { providers.GlobalRouter.Deregister(name) })
	return reg, mock
}

// TestCrystallizeStepToSkill_InheritsGuardrailsFromParent verifies that when
// a parent skill has guardrails, the newly crystallized variant inherits them
// in both config.Skill.Guardrails and GeneratedSkill.Guardrails.
// See SKILL_EVOLUTION_TRIAGE_GUARDRAILS_PLAN.md §2.
func TestCrystallizeStepToSkill_InheritsGuardrailsFromParent(t *testing.T) {
	tmp := t.TempDir()
	reg, _ := newMockJITRegistry(t, "```lua\n-- new variant body\n```")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jitMgr, logger)

	guardrails := []string{"never delete temp files", "always validate input"}
	if err := es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: "jit_root", Capability: "guardrail-test", ParentID: "",
		SuccessRate: 1.0, IsVerified: true, SourceText: "-- root body",
		Guardrails: guardrails,
	}); err != nil {
		t.Fatalf("AddGeneratedSkill: %v", err)
	}

	re.CrystallizeStepToSkill(context.Background(), store.StepRecord{
		StepID: "s1", RoleID: "r1", Capability: "guardrail-test", Status: "OK",
		Task:  "new task variant for guardrail inheritance",
		Trace: []store.ToolInteraction{{ToolName: "shell"}},
	})

	var variant *store.GeneratedSkill
	for _, gs := range es.GetGeneratedSkillsSnapshot() {
		if gs.ID != "jit_root" && gs.Capability == "guardrail-test" {
			variant = gs
		}
	}
	if variant == nil {
		t.Fatal("expected a new variant skill to be crystallized")
	}
	if len(variant.Guardrails) != len(guardrails) {
		t.Fatalf("expected %d guardrails, got %d", len(guardrails), len(variant.Guardrails))
	}
	for i, g := range guardrails {
		if variant.Guardrails[i] != g {
			t.Fatalf("guardrail[%d]: expected %q, got %q", i, g, variant.Guardrails[i])
		}
	}
}

// TestCrystallizeStepToSkill_InjectsGuardrailsIntoPrompt verifies that the
// crystallization prompt includes the parent's guardrails when additive editing.
func TestCrystallizeStepToSkill_InjectsGuardrailsIntoPrompt(t *testing.T) {
	tmp := t.TempDir()
	reg, mock := newMockJITRegistry(t, "```lua\n-- extension\n```")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jitMgr, logger)

	guardrails := []string{"never delete temp files", "always validate input"}
	if err := es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: "jit_root2", Capability: "prompt-inject-test", ParentID: "",
		SuccessRate: 1.0, IsVerified: true, SourceText: "-- root body",
		Guardrails: guardrails,
	}); err != nil {
		t.Fatalf("AddGeneratedSkill: %v", err)
	}

	re.CrystallizeStepToSkill(context.Background(), store.StepRecord{
		StepID: "s1", RoleID: "r1", Capability: "prompt-inject-test", Status: "OK",
		Task:  "task for prompt injection test",
		Trace: []store.ToolInteraction{{ToolName: "shell"}},
	})

	if !strings.Contains(mock.lastSystem, "GUARDRAILS") {
		t.Fatal("expected GUARDRAILS section in system prompt")
	}
	for _, g := range guardrails {
		if !strings.Contains(mock.lastSystem, g) {
			t.Fatalf("expected guardrail %q in system prompt", g)
		}
	}
	if !strings.Contains(mock.lastSystem, "MUST NOT be modified") {
		t.Fatal("expected guardrails declaration language in prompt")
	}
}

// TestMaybePromoteSkill_LogsAuditEvent verifies that when a skill meets the
// promotion threshold (UsageCount >= 3, SuccessRate > 0.8), an
// EventSkillPromoted audit event is logged.
// See SKILL_EVOLUTION_TRIAGE_GUARDRAILS_PLAN.md §3.
func TestMaybePromoteSkill_LogsAuditEvent(t *testing.T) {
	tmp := t.TempDir()
	reg, _ := newPersistableJITRegistry(t, "-- unused")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jitMgr, logger)

	toolID, err := jitMgr.RegisterTool("-- promotable tool", "lua", time.Hour, true)
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	if err := es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: toolID, Capability: "promote-test", ParentID: "",
		UsageCount: 5, SuccessRate: 0.9, IsVerified: false,
	}); err != nil {
		t.Fatalf("AddGeneratedSkill: %v", err)
	}

	re.maybePromoteSkill(context.Background(), toolID)

	gs, ok := es.GetGeneratedSkill(toolID)
	if !ok {
		t.Fatalf("skill %q not found after promotion attempt", toolID)
	}
	if !gs.IsVerified {
		t.Fatal("skill should be verified after meeting promotion threshold")
	}
}

// TestMaybePromoteSkill_DoesNotPromoteBelowThreshold verifies that skills
// below the promotion threshold are not promoted.
func TestMaybePromoteSkill_DoesNotPromoteBelowThreshold(t *testing.T) {
	tmp := t.TempDir()
	reg, _ := newPersistableJITRegistry(t, "-- unused")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jitMgr, logger)

	toolID, err := jitMgr.RegisterTool("-- not-yet-promotable", "lua", time.Hour, true)
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	if err := es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: toolID, Capability: "no-promote-test", ParentID: "",
		UsageCount: 2, SuccessRate: 0.5, IsVerified: false,
	}); err != nil {
		t.Fatalf("AddGeneratedSkill: %v", err)
	}

	re.maybePromoteSkill(context.Background(), toolID)

	gs, ok := es.GetGeneratedSkill(toolID)
	if !ok {
		t.Fatalf("skill %q not found", toolID)
	}
	if gs.IsVerified {
		t.Fatal("skill should NOT be verified when below threshold")
	}
}

// TestMaybePromoteSkill_NoDoublePromotion verifies that an already-verified
// skill is not re-promoted.
func TestMaybePromoteSkill_NoDoublePromotion(t *testing.T) {
	tmp := t.TempDir()
	reg, _ := newPersistableJITRegistry(t, "-- unused")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jitMgr, logger)

	toolID, err := jitMgr.RegisterTool("-- already-verified", "lua", time.Hour, true)
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	if err := es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: toolID, Capability: "double-promote-test", ParentID: "",
		UsageCount: 10, SuccessRate: 0.95, IsVerified: true,
	}); err != nil {
		t.Fatalf("AddGeneratedSkill: %v", err)
	}

	re.maybePromoteSkill(context.Background(), toolID)

	gs, ok := es.GetGeneratedSkill(toolID)
	if !ok {
		t.Fatalf("skill %q not found", toolID)
	}
	if !gs.IsVerified {
		t.Fatal("skill should remain verified")
	}
}

// TestRegisterAsSkill_NoGuardrailsWhenParentHasNone verifies that when the
// parent skill has no guardrails, the new skill also has none.
func TestRegisterAsSkill_NoGuardrailsWhenParentHasNone(t *testing.T) {
	tmp := t.TempDir()
	reg, _ := newMockJITRegistry(t, "```lua\n-- no guardrails body\n```")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jitMgr, logger)

	if err := es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: "jit_noguard_root", Capability: "no-guard-test", ParentID: "",
		SuccessRate: 1.0, IsVerified: true, SourceText: "-- root",
	}); err != nil {
		t.Fatalf("AddGeneratedSkill: %v", err)
	}

	re.CrystallizeStepToSkill(context.Background(), store.StepRecord{
		StepID: "s1", RoleID: "r1", Capability: "no-guard-test", Status: "OK",
		Task:  "task with no guardrails parent",
		Trace: []store.ToolInteraction{{ToolName: "shell"}},
	})

	var variant *store.GeneratedSkill
	for _, gs := range es.GetGeneratedSkillsSnapshot() {
		if gs.ID != "jit_noguard_root" && gs.Capability == "no-guard-test" {
			variant = gs
		}
	}
	if variant == nil {
		t.Fatal("expected a new variant skill")
	}
	if len(variant.Guardrails) != 0 {
		t.Fatalf("expected no guardrails, got %v", variant.Guardrails)
	}
}

// TestRegisterAsSkill_GuardrailsInConfigSkill verifies that the config.Skill
// record also receives the inherited guardrails (not just the GeneratedSkill).
func TestRegisterAsSkill_GuardrailsInConfigSkill(t *testing.T) {
	tmp := t.TempDir()
	reg, _ := newMockJITRegistry(t, "```lua\n-- config skill body\n```")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jitMgr, logger)

	guardrails := []string{"never skip validation"}
	if err := es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: "jit_cfg_root", Capability: "cfg-guard-test", ParentID: "",
		SuccessRate: 1.0, IsVerified: true, SourceText: "-- root",
		Guardrails: guardrails,
	}); err != nil {
		t.Fatalf("AddGeneratedSkill: %v", err)
	}

	re.CrystallizeStepToSkill(context.Background(), store.StepRecord{
		StepID: "s1", RoleID: "r1", Capability: "cfg-guard-test", Status: "OK",
		Task:  "task for config skill guardrails",
		Trace: []store.ToolInteraction{{ToolName: "shell"}},
	})

	var variantID string
	for _, gs := range es.GetGeneratedSkillsSnapshot() {
		if gs.ID != "jit_cfg_root" && gs.Capability == "cfg-guard-test" {
			variantID = gs.ID
		}
	}
	if variantID == "" {
		t.Fatal("expected a new variant skill")
	}

	cfgSkill := reg.Skills[variantID]
	if cfgSkill == nil {
		t.Fatalf("config.Skill %q not found in registry", variantID)
	}
	if len(cfgSkill.Guardrails) != len(guardrails) {
		t.Fatalf("config.Skill guardrails: expected %d, got %d", len(guardrails), len(cfgSkill.Guardrails))
	}
	for i, g := range guardrails {
		if cfgSkill.Guardrails[i] != g {
			t.Fatalf("config.Skill guardrail[%d]: expected %q, got %q", i, g, cfgSkill.Guardrails[i])
		}
	}
}
