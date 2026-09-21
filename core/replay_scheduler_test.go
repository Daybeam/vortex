package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

func setupReplayTestDB(t *testing.T) (*store.Store, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "replay_test.db")
	db, err := store.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	s, err := store.NewStore(t.TempDir(), t.TempDir(), "", &config.SystemSettings{}, db)
	if err != nil {
		db.Close()
		t.Fatalf("NewStore: %v", err)
	}
	return s, func() { db.Close() }
}

func TestScanFailedTasks_FindsFailedTasks(t *testing.T) {
	s, cleanup := setupReplayTestDB(t)
	defer cleanup()
	db := s.DB

	_, _ = db.Exec(`INSERT INTO tasks (task_id, status) VALUES ('failed_1', 'failed')`)
	_, _ = db.Exec(`INSERT INTO tasks (task_id, status) VALUES ('ok_1', 'completed')`)
	_, _ = db.Exec(`INSERT INTO tasks (task_id, status) VALUES ('aborted_1', 'aborted')`)

	rs := NewReplayScheduler(nil, nil, nil, db, nil)
	ids, err := rs.ScanFailedTasks(context.Background(), time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("ScanFailedTasks: %v", err)
	}

	if len(ids) != 2 {
		t.Fatalf("expected 2 failed/aborted task IDs, got %d: %v", len(ids), ids)
	}

	found := make(map[string]bool)
	for _, id := range ids {
		found[id] = true
	}
	if !found["failed_1"] || !found["aborted_1"] {
		t.Errorf("expected failed)failed_1 and aborted_1, got %v", ids)
	}
}

func TestScanFailedTasks_EmptyWhenNoDB(t *testing.T) {
	rs := NewReplayScheduler(nil, nil, nil, nil, nil)
	ids, err := rs.ScanFailedTasks(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 IDs with nil db, got %d", len(ids))
	}
}

func TestMutate_RewriteStrategy_GeneratesVariants(t *testing.T) {
	history := []AgentEvent{
		{TaskID: "T", StepID: "S1", EventType: "step_start"},
		{TaskID: "T", StepID: "S1", EventType: "tool_call", Payload: map[string]any{
			"tool": "custom_tool",
			"args": map[string]any{"command": "ls -la"},
		}},
	}

	rs := NewReplayScheduler(nil, nil, nil, nil, nil)
	variants, err := rs.Mutate(history, "S1")
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	if len(variants) != 3 {
		t.Fatalf("expected 3 variants for tool without equivalent, got %d", len(variants))
	}

	for _, v := range variants {
		if v.Type != "rewrite" {
			t.Errorf("variant type = %s, want rewrite", v.Type)
		}
		if v.Payload == nil {
			t.Error("variant payload should not be nil")
		}
	}

	if _, ok := variants[0].Payload["exit_criteria"]; !ok {
		t.Error("variant 0 should have exit_criteria")
	}
	if _, ok := variants[1].Payload["_mutation"]; !ok {
		t.Error("variant 1 should have _mutation tag")
	}
}

func TestMutate_NoToolCall_ReturnsEmpty(t *testing.T) {
	history := []AgentEvent{
		{TaskID: "T", StepID: "S1", EventType: "step_start"},
	}

	rs := NewReplayScheduler(nil, nil, nil, nil, nil)
	variants, err := rs.Mutate(history, "S1")
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if len(variants) != 0 {
		t.Errorf("expected 0 variants without tool_call, got %d", len(variants))
	}
}

func TestFindFailedStep_FindsErrorEvent(t *testing.T) {
	history := []AgentEvent{
		{StepID: "S1", EventType: "step_start"},
		{StepID: "S1", EventType: "tool_call"},
		{StepID: "S2", EventType: "step_start"},
		{StepID: "S2", EventType: "tool_error"},
	}

	got := findFailedStep(history)
	if got != "S2" {
		t.Errorf("findFailedStep = %s, want S2", got)
	}
}

func TestFindFailedStep_FindsStepFailedEvent(t *testing.T) {
	history := []AgentEvent{
		{StepID: "S1", EventType: "step_start"},
		{StepID: "S1", EventType: "step_failed"},
	}

	got := findFailedStep(history)
	if got != "S1" {
		t.Errorf("findFailedStep = %s, want S1", got)
	}
}

func TestRunReplayCycle_EndToEnd(t *testing.T) {
	s, cleanup := setupReplayTestDB(t)
	defer cleanup()
	db := s.DB

	_, _ = db.Exec(`INSERT INTO tasks (task_id, status) VALUES ('replay_task_1', 'failed')`)

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "global_trajectory.jsonl")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	events := []AgentEvent{
		{TaskID: "replay_task_1", StepID: "S1", EventType: "step_start"},
		{TaskID: "replay_task_1", StepID: "S1", EventType: "tool_call", Payload: map[string]any{
			"tool": "exec_command",
			"args": map[string]any{"command": "ls"},
		}},
		{TaskID: "replay_task_1", StepID: "S1", EventType: "tool_error"},
	}
	for _, ev := range events {
		data, _ := json.Marshal(ev)
		f.Write(data)
		f.Write([]byte("\n"))
	}
	f.Close()

	replayer := NewReplayer(logPath)
	expStore, ok := s.Experience.(*store.ExperienceStore)
	if !ok {
		t.Fatal("type assertion to *ExperienceStore failed")
	}
	rs := NewReplayScheduler(replayer, nil, expStore, db, nil)

	if err := rs.RunReplayCycle(context.Background()); err != nil {
		t.Fatalf("RunReplayCycle: %v", err)
	}

	var count int
	db.QueryRow("SELECT COUNT(*) FROM replay_mutation_records WHERE source_task_id = 'replay_task_1'").Scan(&count)
	if count != 4 {
		t.Errorf("expected 4 mutation records persisted (3 rewrite + 1 toolchain), got %d", count)
	}

	var candidateCount int
	db.QueryRow("SELECT COUNT(*) FROM replay_mutation_records WHERE candidate_id != '' AND verifier_result = 'skipped'").Scan(&candidateCount)
	if candidateCount != 4 {
		t.Errorf("expected 4 records with candidate_id (skipped verification), got %d", candidateCount)
	}
}

func TestRunReplayCycle_NoFailedTasks_NoOp(t *testing.T) {
	s, cleanup := setupReplayTestDB(t)
	defer cleanup()

	replayer := NewReplayer(filepath.Join(t.TempDir(), "empty.jsonl"))
	expStore, _ := s.Experience.(*store.ExperienceStore)
	rs := NewReplayScheduler(replayer, nil, expStore, s.DB, nil)

	if err := rs.RunReplayCycle(context.Background()); err != nil {
		t.Fatalf("RunReplayCycle: %v", err)
	}

	var count int
	s.DB.QueryRow("SELECT COUNT(*) FROM replay_mutation_records").Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 mutation records with no failed tasks, got %d", count)
	}
}

func TestMutate_AlternativeToolchain(t *testing.T) {
	history := []AgentEvent{
		{TaskID: "T", StepID: "S1", EventType: "tool_call", Payload: map[string]any{
			"tool": "exec_command",
			"args": map[string]any{"command": "ls"},
		}},
	}

	rs := NewReplayScheduler(nil, nil, nil, nil, nil)
	variants, err := rs.Mutate(history, "S1")
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	var foundToolchain bool
	for _, v := range variants {
		if v.Type == "toolchain" {
			foundToolchain = true
			alt, _ := v.Payload["_tool"].(string)
			if alt != "orchestrator_invoke" {
				t.Errorf("toolchain swap: expected orchestrator_invoke, got %s", alt)
			}
		}
	}
	if !foundToolchain {
		t.Error("expected a toolchain mutation variant for exec_command")
	}
}

func TestMutate_ParamPerturbation(t *testing.T) {
	history := []AgentEvent{
		{TaskID: "T", StepID: "S1", EventType: "tool_call", Payload: map[string]any{
			"tool": "exec_command",
			"args": map[string]any{"retries": 3, "timeout": 30},
		}},
	}

	rs := NewReplayScheduler(nil, nil, nil, nil, nil)
	variants, err := rs.Mutate(history, "S1")
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	var foundPerturbation bool
	for _, v := range variants {
		if v.Type == "perturbation" {
			foundPerturbation = true
			retries, ok := v.Payload["retries"].(int)
			if !ok {
				t.Error("perturbation should preserve int type for retries")
			}
			if retries == 3 {
				t.Error("perturbation should have changed retries from original 3")
			}
		}
	}
	if !foundPerturbation {
		t.Error("expected a perturbation mutation variant for numeric args")
	}
}

func TestAlternativeTool_KnownEquivalents(t *testing.T) {
	cases := []struct{ in, want string }{
		{"exec_command", "orchestrator_invoke"},
		{"write_file", "patch"},
		{"read_file", "grep"},
		{"unknown_tool", ""},
	}
	for _, c := range cases {
		got := alternativeTool(c.in)
		if got != c.want {
			t.Errorf("alternativeTool(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPerturbParams_NoNumerics(t *testing.T) {
	args := map[string]any{"command": "ls", "path": "/tmp"}
	result := perturbParams(args)
	if result != nil {
		t.Error("expected nil for non-numeric params")
	}
}

func TestReplaySandbox_DirectExecute(t *testing.T) {
	sandbox := NewReplaySandbox()
	result, err := sandbox.DirectExecute(context.Background(), "replay", "any_tool", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatal("expected map[string]any result")
	}
	if m["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", m["status"])
	}
	if m["sandbox"] != true {
		t.Error("expected sandbox=true in result")
	}
}

func TestGrayscaleController_PromotionGate(t *testing.T) {
	gc := NewGrayscaleController(1.0, 3)

	for i := 0; i < 3; i++ {
		gc.RecordOutcome("cand_1", true)
	}

	if !gc.ShouldPromote("cand_1") {
		t.Error("expected promotion after 3 consecutive successes")
	}
	if gc.ShouldPromote("cand_1") {
		t.Error("should not promote twice")
	}
}

func TestGrayscaleController_ArchiveOnFailure(t *testing.T) {
	gc := NewGrayscaleController(1.0, 10)

	gc.RecordOutcome("cand_2", true)
	gc.RecordOutcome("cand_2", true)
	gc.RecordOutcome("cand_2", false)

	if !gc.IsArchived("cand_2") {
		t.Error("expected archived after failure")
	}
	if gc.ShouldPromote("cand_2") {
		t.Error("archived candidate should not be promotable")
	}
}

func TestGrayscaleController_ShouldTrial(t *testing.T) {
	gc := NewGrayscaleController(1.0, 10)

	if !gc.ShouldTrial("cand_3") {
		t.Error("with trialRate=1.0, ShouldTrial should always return true")
	}

	gc2 := NewGrayscaleController(0.0, 10)
	if gc2.ShouldTrial("cand_4") {
		t.Error("with trialRate=0.0, ShouldTrial should return false for new candidate")
	}
}

func TestVerifyMutation_NoVerifier_NoCrossFamily_ReturnsNoVerifier(t *testing.T) {
	rs := NewReplayScheduler(nil, nil, nil, nil, nil)
	v := MutationVariant{Type: "rewrite", Payload: map[string]any{"_tool": "exec_command"}}
	ok, reason := rs.verifyMutation(context.Background(), v)
	if ok {
		t.Error("expected ok=false with no verifier")
	}
	if reason != "no_verifier" {
		t.Errorf("expected reason='no_verifier', got %q", reason)
	}
}

func TestVerifyMutation_NoVerifier_CrossFamilyReturnsEmpty_ReturnsNoVerifier(t *testing.T) {
	rs := NewReplayScheduler(nil, nil, nil, nil, nil)
	rs.SetCrossFamilyVerifier(func() string { return "" }, nil)
	v := MutationVariant{Type: "rewrite", Payload: map[string]any{"_tool": "exec_command"}}
	ok, reason := rs.verifyMutation(context.Background(), v)
	if ok {
		t.Error("expected ok=false when cross-family resolver returns empty")
	}
	if reason != "no_verifier" {
		t.Errorf("expected reason='no_verifier', got %q", reason)
	}
}

func TestVerifyMutation_NoVerifier_CrossFamilySet_ButNoSpawner_ReturnsNoVerifier(t *testing.T) {
	rs := NewReplayScheduler(nil, nil, nil, nil, nil)
	rs.SetCrossFamilyVerifier(func() string { return "cross_family_provider" }, nil)
	v := MutationVariant{Type: "rewrite", Payload: map[string]any{"_tool": "exec_command"}}
	ok, reason := rs.verifyMutation(context.Background(), v)
	if ok {
		t.Error("expected ok=false when spawner is nil")
	}
	if reason != "no_verifier" {
		t.Errorf("expected reason='no_verifier', got %q", reason)
	}
}

func TestSetCrossFamilyVerifier_SetsFields(t *testing.T) {
	rs := NewReplayScheduler(nil, nil, nil, nil, nil)
	resolver := func() string { return "test_provider" }
	rs.SetCrossFamilyVerifier(resolver, nil)
	if rs.crossFamilyResolver == nil {
		t.Error("expected crossFamilyResolver to be set")
	}
	if rs.crossFamilyResolver() != "test_provider" {
		t.Error("expected resolver to return 'test_provider'")
	}
}
