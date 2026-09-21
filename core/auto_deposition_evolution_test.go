package core

import (
	"context"
	"os"
	"testing"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

type mockAntiPatternBackend struct {
	upserted []store.AntiPatternPrecedent
}

func (m *mockAntiPatternBackend) Upsert(ctx context.Context, p store.AntiPatternPrecedent) error {
	m.upserted = append(m.upserted, p)
	return nil
}

func (m *mockAntiPatternBackend) LoadAll(ctx context.Context) ([]store.AntiPatternPrecedent, error) {
	return nil, nil
}

func TestDepositExperienceEvolution(t *testing.T) {
	backend := &mockAntiPatternBackend{}
	apStore := store.NewAntiPatternStore(nil, backend)
	expStore := &store.ExperienceStore{
		AntiPatternStore: apStore,
	}

	tmpDir, _ := os.MkdirTemp("", "logtest")
	defer os.RemoveAll(tmpDir)
	logger, _ := NewLogger(tmpDir, nil)
	defer logger.Close()

	engine := &DirectedEngine{
		expStore: expStore,
		logger:   logger,
	}

	graph := &schemas.TaskGraph{
		TaskID: "t1",
	}

	step := &schemas.Step{
		ID:                      "s1",
		RoleID:                  "test_role",
		Task:                    "Test Task for Evolution",
		TriggerError:            "Anchor drift in large file",
		AdditionalPromptContext: "Use targeted offset instead of full patch",
	}

	engine.DepositExperienceEvolution(context.Background(), graph, step)

	if len(backend.upserted) == 0 {
		t.Fatal("expected one anti-pattern to be deposited")
	}

	p := backend.upserted[0]
	if p.AntiPattern != "Pitfall in s1 (test_role)" {
		t.Errorf("expected AntiPattern 'Pitfall in s1 (test_role)', got '%s'", p.AntiPattern)
	}
	if p.CorrectPattern != step.AdditionalPromptContext {
		t.Errorf("expected CorrectPattern match, got '%s'", p.CorrectPattern)
	}
	if p.Symptom != step.TriggerError {
		t.Errorf("expected Symptom match, got '%s'", p.Symptom)
	}
}
