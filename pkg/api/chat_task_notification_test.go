package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// Regression test for what logs/2026-11-06/task_75f448f3.jsonl showed end to
// end: the delegated step ended status="partial", confidence=0.30 and wrote no
// artifact, while the graph still reached "completed". The summary branches
// treated that as the success case, so the user was told "completed
// successfully" and the follow-up turn -- which reads only this string -- was
// handed a success verdict with no deliverable in it and answered from its own
// priors. The binary verdict also dropped output_files and result_ref, which
// until now travelled only in ChatEvent.Meta, a field Telegram never reads.

func confPtr(v float64) *float64 { return &v }

func TestTaskOutcomeSummary_PartialStepIsNotSuccess(t *testing.T) {
	o := taskOutcome{
		Desc:      "深度分析 livekit/agents",
		ID:        "task_75f448f3",
		Status:    string(schemas.GraphCompleted),
		EventType: string(obs.EventTaskCompleted),
		GapStep:   "s1",
		GapConf:   confPtr(0.30),
		GapNotes:  "未克隆仓库，只查了一个 API 端点",
	}
	got := o.summary()

	if strings.Contains(got, "completed successfully") {
		t.Errorf("a partial step must not be announced as success:\n%s", got)
	}
	for _, want := range []string{"finished with gaps", "s1", "0.30", "No output files", "未克隆仓库"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestTaskOutcomeSummary_NamesTheDeliverable(t *testing.T) {
	o := taskOutcome{
		Desc:      "audit the repo",
		ID:        "task_abc",
		Status:    string(schemas.GraphCompleted),
		EventType: string(obs.EventTaskCompleted),
		Files: []map[string]any{
			{"path": "outputs/task_abc/report.md", "is_primary": true},
			{"path": "outputs/task_abc/findings.json", "is_primary": false},
		},
	}
	got := o.summary()
	if !strings.Contains(got, "completed successfully") {
		t.Errorf("clean completion should still read as success:\n%s", got)
	}
	if !strings.Contains(got, "outputs/task_abc/report.md (primary)") {
		t.Errorf("summary should list the primary artifact:\n%s", got)
	}
}

func TestTaskOutcomeSummary_ReportsFailureAndError(t *testing.T) {
	o := taskOutcome{
		Desc:      "audit the repo",
		ID:        "task_abc",
		Status:    string(schemas.GraphFailed),
		EventType: string(obs.EventTaskFailed),
		LastError: "provider unavailable",
	}
	got := o.summary()
	if !strings.Contains(got, "failed during execution") || !strings.Contains(got, "provider unavailable") {
		t.Errorf("failure should stay a failure and carry the reason:\n%s", got)
	}
}

func TestTaskOutcomeSummary_FallsBackToResultRef(t *testing.T) {
	o := taskOutcome{
		Desc:      "audit the repo",
		ID:        "task_abc",
		Status:    string(schemas.GraphCompleted),
		EventType: string(obs.EventTaskCompleted),
		ResultRef: "outputs/task_abc/.staging/art_123.txt",
	}
	got := o.summary()
	if !strings.Contains(got, "art_123.txt") {
		t.Errorf("with no output files the result reference is the only pointer the reader gets:\n%s", got)
	}
}

// TestCollectTaskOutcome reads the scheduler's status view the way
// ToStatusDictView writes it: steps keyed by id, confidence as *float64,
// missing_context as []string.
func TestCollectTaskOutcome_ReadsTheStatusView(t *testing.T) {
	statusData := map[string]any{
		"status": string(schemas.GraphCompleted),
		"steps": map[string]any{
			"s1": map[string]any{
				"status":          string(schemas.StepPartial),
				"confidence":      confPtr(0.30),
				"missing_context": []string{"未克隆仓库", "未验证 WebRTC 路径"},
			},
		},
		"output_files": []map[string]any{},
	}

	o := collectTaskOutcome("task_75f448f3", "深度分析 livekit/agents", string(obs.EventTaskCompleted), statusData)
	if o.GapStep != "s1" || o.GapConf == nil || *o.GapConf != 0.30 {
		t.Fatalf("gap step not collected: %+v", o)
	}
	if !strings.Contains(o.GapNotes, "未验证 WebRTC 路径") {
		t.Errorf("missing_context must carry through, got %q", o.GapNotes)
	}
	if got := o.summary(); !strings.Contains(got, "finished with gaps") {
		t.Errorf("summary from collected outcome:\n%s", got)
	}
}

func TestCollectTaskOutcome_PicksTheWeakestPartialStep(t *testing.T) {
	statusData := map[string]any{
		"status": string(schemas.GraphCompleted),
		"steps": map[string]any{
			"s1": map[string]any{"status": string(schemas.StepPartial)},
			"s2": map[string]any{"status": string(schemas.StepPartial), "confidence": confPtr(0.20)},
			"s3": map[string]any{"status": string(schemas.StepOK), "confidence": confPtr(0.9)},
		},
	}

	o := collectTaskOutcome("task_x", "x", string(obs.EventTaskCompleted), statusData)
	if o.GapStep != "s2" || o.GapConf == nil || *o.GapConf != 0.20 {
		t.Fatalf("the lowest known confidence should be the one named, got step=%q conf=%v", o.GapStep, o.GapConf)
	}
}

// contractTask is a delegated step prompt in the shape the benchmark controller
// appends: the files the workspace must contain live in the contract section.
const contractTask = "write the report\n\n## Enforced workspace change contract\n\n" +
	"- Create: `report.md`, `findings.json`\n- Modify: none\n- Delete: none\n"

// Regression test for the delivery gap measured on chat delegations: the
// sub-agent writes its deliverable with file tools, which never reach
// graph.OutputFiles (that list is appended only from a step's structured
// output_ref, scheduler_decision_success.go:126). The notification therefore
// said "No output files were produced" over a workspace that had the files.
// The status view does carry workspace_root and the step task text, so the
// notification must check the declared contract before claiming nothing exists.
func TestTaskOutcomeSummary_ListsDeclaredArtifactsTheGraphNeverSaw(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "report.md"), []byte("# done"), 0o644); err != nil {
		t.Fatal(err)
	}

	statusData := map[string]any{
		"status":         string(schemas.GraphCompleted),
		"workspace_root": root,
		"steps": map[string]any{
			"s1": map[string]any{
				"status": string(schemas.StepOK),
				"task":   contractTask,
			},
		},
		"output_files": []map[string]any{},
	}

	o := collectTaskOutcome("task_delivery", "write the report", string(obs.EventTaskCompleted), statusData)
	got := o.summary()
	if strings.Contains(got, "No output files") {
		t.Errorf("the graph saw no output_ref, but the deliverable is on disk:\n%s", got)
	}
	if !strings.Contains(got, "report.md") {
		t.Errorf("the present artifact must be named:\n%s", got)
	}
	if !strings.Contains(got, "Still missing: findings.json") {
		t.Errorf("the missing half of the contract must be named too:\n%s", got)
	}
}

func TestTaskOutcomeSummary_DeclaredContractWithNothingOnDisk(t *testing.T) {
	statusData := map[string]any{
		"status":         string(schemas.GraphCompleted),
		"workspace_root": t.TempDir(),
		"steps": map[string]any{
			"s1": map[string]any{
				"status": string(schemas.StepOK),
				"task":   contractTask,
			},
		},
	}

	got := collectTaskOutcome("task_empty", "write the report", string(obs.EventTaskCompleted), statusData).summary()
	if !strings.Contains(got, "No declared artifacts were produced") {
		t.Errorf("an empty workspace must say so about the declared names:\n%s", got)
	}
	for _, want := range []string{"report.md", "findings.json"} {
		if !strings.Contains(got, want) {
			t.Errorf("the declared name %q must appear so the reader knows what was asked for:\n%s", want, got)
		}
	}
}

// Without a contract there is no name to stat, so the notification must not
// claim "none": it names the workspace instead, which is the only thing it
// actually knows.
func TestTaskOutcomeSummary_NoContractNamesTheWorkspace(t *testing.T) {
	root := t.TempDir()
	statusData := map[string]any{
		"status":         string(schemas.GraphCompleted),
		"workspace_root": root,
		"steps": map[string]any{
			"s1": map[string]any{"status": string(schemas.StepOK), "task": "free-form work"},
		},
	}

	got := collectTaskOutcome("task_free", "free-form work", string(obs.EventTaskCompleted), statusData).summary()
	if strings.Contains(got, "No output files were produced") {
		t.Errorf("the graph cannot see file-tool writes, so 'none produced' is a lie:\n%s", got)
	}
	if !strings.Contains(got, root) {
		t.Errorf("the workspace root is the only pointer the reader has:\n%s", got)
	}
}
