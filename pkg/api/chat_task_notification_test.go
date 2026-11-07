package api

import (
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
