package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// TestReflectOnTask_CreditsDynamicSkillExposure is the end-to-end half of the
// JIT reusability loop: a skill retrieved for a step is attached to the spawn
// prompt only, so before this handoff StepRecord.Skills never contained it and
// UsageCount stayed 0 forever -- making maybePromoteSkill's UsageCount>=3 gate
// unreachable. ReflectOnTask must fold the recorded exposure into the step's
// skill list so RecordTaskCompletion credits it.
// Regression for JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §7.4.
func TestReflectOnTask_CreditsDynamicSkillExposure(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "reflect-exposure-")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	reg, err := config.NewRegistry(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	loggerDir, err := os.MkdirTemp("", "reflect-exposure-log-")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	logger := mustNewLogger(t, loggerDir, nil)
	defer logger.Close()

	es := mustNewExperienceStore(t, tmpDir, nil, nil, nil, nil)
	re := NewReflectionEngine(es, &mockSignalTaskStore{}, reg, nil, logger)

	if err := es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: "jit_exposed", Capability: "analyze", SuccessRate: 1.0,
	}); err != nil {
		t.Fatalf("AddGeneratedSkill: %v", err)
	}

	graph := &schemas.TaskGraph{
		TaskID: "task_exposure",
		Status: schemas.GraphCompleted,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "r1", Task: "test task", Status: schemas.StepOK},
		},
	}
	es.RecordSkillExposure(graph.TaskID, "s1", []string{"jit_exposed"})

	re.ReflectOnTask(context.Background(), graph)
	es.WaitAsyncSaves()

	gs, ok := es.GetGeneratedSkill("jit_exposed")
	if !ok {
		t.Fatal("jit_exposed disappeared")
	}
	if gs.UsageCount != 1 {
		t.Fatalf("exposure must reach StepRecord.Skills and credit UsageCount, got %d", gs.UsageCount)
	}
}
