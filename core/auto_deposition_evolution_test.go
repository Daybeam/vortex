package core

import (
	"context"
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

	tmpDir := t.TempDir()
	logger := mustNewLogger(t, tmpDir, nil)
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

// TestDepositExperienceEvolution_EarlyReturns covers the guard clauses
// in DepositExperienceEvolution that cause early return without depositing.
//
// audit TEST-EARLY-RETURNS (2026-10-23): the original body ran the four guard
// cases but asserted NOTHING ("the fact that we didn't panic is sufficient"),
// so it would still pass if every guard clause were deleted. It now wires a
// mock backend into the two cases where one is reachable and asserts that no
// anti-pattern was deposited.
func TestDepositExperienceEvolution_EarlyReturns(t *testing.T) {
	tmpDir := t.TempDir()
	logger := mustNewLogger(t, tmpDir, nil)
	defer logger.Close()

	graph := &schemas.TaskGraph{TaskID: "t1"}

	// Backends for the two cases whose guard can still be observed: the guard
	// must return BEFORE depositing, so the backend must stay empty.
	emptyTriggerBackend := &mockAntiPatternBackend{}
	emptyTriggerEngine := &DirectedEngine{
		expStore: &store.ExperienceStore{
			AntiPatternStore: store.NewAntiPatternStore(nil, emptyTriggerBackend),
		},
		logger: logger,
	}
	emptyContextBackend := &mockAntiPatternBackend{}
	emptyContextEngine := &DirectedEngine{
		expStore: &store.ExperienceStore{
			AntiPatternStore: store.NewAntiPatternStore(nil, emptyContextBackend),
		},
		logger: logger,
	}

	tests := []struct {
		name    string
		engine  *DirectedEngine
		step    *schemas.Step
		backend *mockAntiPatternBackend // nil when no backend is reachable
	}{
		{
			name: "nil_expStore_returns_early",
			engine: &DirectedEngine{
				expStore: nil,
				logger:   logger,
			},
			step: &schemas.Step{
				ID:                      "s1",
				TriggerError:            "some error",
				AdditionalPromptContext: "some fix",
			},
		},
		{
			name:   "empty_trigger_error_returns_early",
			engine: emptyTriggerEngine,
			step: &schemas.Step{
				ID:                      "s1",
				TriggerError:            "",
				AdditionalPromptContext: "some fix",
			},
			backend: emptyTriggerBackend,
		},
		{
			name:   "empty_additional_context_returns_early",
			engine: emptyContextEngine,
			step: &schemas.Step{
				ID:                      "s1",
				TriggerError:            "some error",
				AdditionalPromptContext: "",
			},
			backend: emptyContextBackend,
		},
		{
			name: "nil_antipattern_store_returns_early",
			engine: &DirectedEngine{
				expStore: &store.ExperienceStore{
					AntiPatternStore: nil,
				},
				logger: logger,
			},
			step: &schemas.Step{
				ID:                      "s1",
				TriggerError:            "some error",
				AdditionalPromptContext: "some fix",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Must not panic...
			tt.engine.DepositExperienceEvolution(context.Background(), graph, tt.step)

			// ...and must NOT deposit when the guard clause fired. Pre-fix this
			// assertion did not exist, so a deleted guard would go unnoticed.
			if tt.backend != nil && len(tt.backend.upserted) != 0 {
				t.Errorf("guard clause deposited %d anti-pattern(s); expected 0", len(tt.backend.upserted))
			}
		})
	}
}

// TestDepositExperienceEvolution_LongIntentTruncation verifies that the
// intent (step.Task) is truncated to 100 characters when it exceeds that
// length, as per the code: `if len(intent) > 100 { intent = intent[:100] }`.
func TestDepositExperienceEvolution_LongIntentTruncation(t *testing.T) {
	backend := &mockAntiPatternBackend{}
	apStore := store.NewAntiPatternStore(nil, backend)
	expStore := &store.ExperienceStore{
		AntiPatternStore: apStore,
	}

	tmpDir := t.TempDir()
	logger := mustNewLogger(t, tmpDir, nil)
	defer logger.Close()

	engine := &DirectedEngine{
		expStore: expStore,
		logger:   logger,
	}

	graph := &schemas.TaskGraph{TaskID: "t1"}

	// Create a task string longer than 100 characters.
	longTask := ""
	for i := 0; i < 200; i++ {
		longTask += "x"
	}

	step := &schemas.Step{
		ID:                      "s1",
		RoleID:                  "test_role",
		Task:                    longTask,
		TriggerError:            "Anchor drift",
		AdditionalPromptContext: "Use targeted offset",
	}

	engine.DepositExperienceEvolution(context.Background(), graph, step)

	if len(backend.upserted) == 0 {
		t.Fatal("expected one anti-pattern to be deposited")
	}

	p := backend.upserted[0]
	// The TriggerCondition field is formatted as "Task: <intent>".
	// With truncation, intent should be 100 chars, so TriggerCondition
	// should be "Task: " (6 chars) + 100 chars = 106 chars.
	expectedLen := len("Task: ") + 100
	if len(p.TriggerCondition) != expectedLen {
		t.Errorf("expected TriggerCondition length %d (truncated intent), got %d: %q",
			expectedLen, len(p.TriggerCondition), p.TriggerCondition)
	}
}
