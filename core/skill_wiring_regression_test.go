package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestExpandSkillsIfNecessary_WiresDirectorySkill is a regression guard for
// PRD §2.2 (Skill Interpreter). A DirectorySkill bound to the entry role must
// expand into a multi-step DAG. Before expandSkillsIfNecessary was wired into
// SubmitWithSessionIR it had zero callers, so DirectorySkill entry points were
// never expanded (the interpreter was dead despite the PRD documenting it as
// integrated at the scheduler submit path).
func TestExpandSkillsIfNecessary_WiresDirectorySkill(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "dir_skill")
	promptsDir := filepath.Join(skillDir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "step1.md"), []byte("step one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "step2.md"), []byte("step two"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.NewRegistry(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg.Mu.Lock()
	reg.Skills["dir_skill"] = &config.Skill{
		ID:            "dir_skill",
		ExecutionMode: config.ExecutionModeDirectory,
		SkillDir:      skillDir,
	}
	reg.Roles["dir_expert"] = &config.Role{ID: "dir_expert", BoundSkills: []string{"dir_skill"}}
	reg.Mu.Unlock()

	logger := mustNewLogger(t, t.TempDir(), nil)
	t.Cleanup(func() { logger.Close() })

	engine := &DirectedEngine{registry: reg, logger: logger}

	inputs := []schemas.StepInput{{ID: "entry", RoleID: "dir_expert", Task: "run skill"}}
	out, err := engine.expandSkillsIfNecessary(inputs, nil, nil)
	if err != nil {
		t.Fatalf("expandSkillsIfNecessary: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 expanded steps, got %d", len(out))
	}
	if out[0].ID != "step1" || out[1].ID != "step2" {
		t.Fatalf("unexpected step IDs: %s, %s", out[0].ID, out[1].ID)
	}
	if len(out[1].DependsOn) != 1 || out[1].DependsOn[0] != "step1" {
		t.Fatalf("expected step2 to depend on step1, got %v", out[1].DependsOn)
	}
}

// TestChatSessionStore_SetLifecycleContext_PropagatesToDBCtx is a regression
// guard for audit C-12. Commit 3fcf97bcf added SetLifecycleContext and made
// dbCtx() derive from it, but never wired the call site, so lifecycleCtx
// stayed nil and dbCtx() silently fell back to context.Background() — leaving
// shutdown unable to cancel chat-session DB ops. This pins the contract that
// a set lifecycle ctx actually propagates to dbCtx().
func TestChatSessionStore_SetLifecycleContext_PropagatesToDBCtx(t *testing.T) {
	st := NewChatSessionStore(t.TempDir(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	st.SetLifecycleContext(ctx)
	cancel() // simulate shutdown

	dbCtx, dbCancel := st.dbCtx()
	defer dbCancel()
	select {
	case <-dbCtx.Done():
		// expected: dbCtx inherits the cancelled lifecycleCtx
	case <-time.After(time.Second):
		t.Fatal("dbCtx() did not inherit the cancelled lifecycleCtx (C-12 wiring missing)")
	}
}

// TestPromptAssembler_DirectorySkillInjectsLightweightMetadata is a regression
// guard for B1 (buildSkillMetadata wiring). A DirectorySkill must get a
// lightweight metadata block in the system prompt, not the full skill prompt,
// because expandSkillsIfNecessary already expanded it into sub-steps. Without
// this branch, the LLM sees both the full spec (system prompt) and the
// individual steps (task input) — redundant context bloat.
func TestPromptAssembler_DirectorySkillInjectsLightweightMetadata(t *testing.T) {
	reg := &config.Registry{
		Skills: map[string]*config.Skill{
			"dir_skill": {
				ID:            "dir_skill",
				Name:          "Code Review Pipeline",
				Description:   "Multi-step code review",
				ExecutionMode: config.ExecutionModeDirectory,
				Implementations: map[string]config.SkillImplementation{
					"default": {SystemPrompt: "FULL_SKILL_PROMPT_THAT_SHOULD_NOT_APPEAR"},
				},
			},
		},
	}
	logger := mustNewLogger(t, t.TempDir(), &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() })
	spawner := NewSpawner(reg, nil, nil, logger, nil, "outputs")
	hub := NewContextHub(reg, nil, nil)

	blocks, err := spawner.buildSystemPrompt(
		hub, nil, []string{"dir_skill"}, "coding",
		&config.ProviderConfig{Model: "test-model"}, []string{}, nil, nil, false, nil,
		"review the code", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	var full strings.Builder
	for _, b := range blocks {
		full.WriteString(b.Text)
	}
	output := full.String()

	if !strings.Contains(output, "[Directory-Mode]") {
		t.Error("expected [Directory-Mode] metadata marker in prompt, got none")
	}
	if !strings.Contains(output, "Code Review Pipeline") {
		t.Error("expected skill name in metadata block")
	}
	if strings.Contains(output, "FULL_SKILL_PROMPT_THAT_SHOULD_NOT_APPEAR") {
		t.Error("DirectorySkill full prompt was injected — should use lightweight metadata instead")
	}
}

// TestReplayScheduler_StableCandidateID is a regression guard for B4.
// Before the fix, processMutation generated a fresh jit_<uuid> per mutation,
// so GrayscaleController could never track a candidate across replay cycles
// (ShouldPromote could never reach 10 consecutive successes). After the fix,
// the candidate ID is a deterministic hash of (taskID, stepID, type, payload),
// so the same mutation across cycles gets the same ID.
func TestReplayScheduler_StableCandidateID(t *testing.T) {
	rs := &replayScheduler{
		grayscale: NewGrayscaleController(0.05, 10),
	}

	v := MutationVariant{Type: "param_tweak", Payload: map[string]any{"_tool": "read_file"}}

	r1 := rs.processMutation(context.Background(), "task-1", "step-1", "do stuff", v)
	r2 := rs.processMutation(context.Background(), "task-1", "step-1", "do stuff", v)

	if r1.CandidateID == "" {
		t.Fatal("expected non-empty candidate ID")
	}
	if r1.CandidateID != r2.CandidateID {
		t.Fatalf("same mutation should produce same candidate ID: got %q and %q", r1.CandidateID, r2.CandidateID)
	}

	v2 := MutationVariant{Type: "tool_swap", Payload: map[string]any{"_tool": "write_file"}}
	r3 := rs.processMutation(context.Background(), "task-1", "step-1", "do stuff", v2)
	if r3.CandidateID == r1.CandidateID {
		t.Fatal("different mutation should produce different candidate ID")
	}
}

// TestReplayScheduler_GrayscaleArchivesFailedCandidate is a regression guard
// for B4. When a mutation fails verification, RecordOutcome(verified=false)
// archives the candidate. The next processMutation with the same mutation
// identity should be skipped by IsArchived.
func TestReplayScheduler_GrayscaleArchivesFailedCandidate(t *testing.T) {
	gc := NewGrayscaleController(0.05, 10)
	rs := &replayScheduler{
		grayscale: gc,
		verifier:  &failingVerifier{},
	}

	v := MutationVariant{Type: "param_tweak", Payload: map[string]any{"_tool": "read_file"}}

	r1 := rs.processMutation(context.Background(), "task-1", "step-1", "do stuff", v)
	if r1.VerifierResult != "rejected" {
		t.Fatalf("expected rejected, got %q", r1.VerifierResult)
	}

	r2 := rs.processMutation(context.Background(), "task-1", "step-1", "do stuff", v)
	if r2.VerifierResult != "archived" {
		t.Fatalf("expected archived (skipped by grayscale), got %q", r2.VerifierResult)
	}
}

type failingVerifier struct{}

func (f *failingVerifier) Sense(ctx context.Context, mcpID, toolName string, executor ToolExecutor) (VerificationState, error) {
	return nil, nil
}
func (f *failingVerifier) Verify(ctx context.Context, mcpID, toolName string, before VerificationState, result any, executor ToolExecutor) (bool, error) {
	return false, nil
}
func (f *failingVerifier) Recover(ctx context.Context, mcpID, toolName string, args map[string]any, err error, executor ToolExecutor) (any, error) {
	return nil, nil
}
