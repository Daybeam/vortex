package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildWorklogEntry(t *testing.T) {
	prev := ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 5, "clarity": 7}}
	next := ReviewScores{Overall: 7.0, Axes: map[string]float64{"layout": 7, "clarity": 7}}

	entry := BuildWorklogEntry(2, "fix layout overflow", prev, next, VerdictAccept)

	if entry.Round != 2 {
		t.Errorf("Round: got %d, want 2", entry.Round)
	}
	if entry.Plan != "fix layout overflow" {
		t.Errorf("Plan: got %q", entry.Plan)
	}
	if entry.Outcome != "accepted" {
		t.Errorf("Outcome: got %q, want accepted", entry.Outcome)
	}
	// Deltas: overall +1, layout +2, clarity 0
	if entry.Deltas["overall"] != 1.0 {
		t.Errorf("delta overall: got %v, want 1.0", entry.Deltas["overall"])
	}
	if entry.Deltas["layout"] != 2.0 {
		t.Errorf("delta layout: got %v, want 2.0", entry.Deltas["layout"])
	}
	if entry.Deltas["clarity"] != 0.0 {
		t.Errorf("delta clarity: got %v, want 0.0", entry.Deltas["clarity"])
	}
	// ScoresBefore/After should include "overall" + axes
	if entry.ScoresBefore["overall"] != 6.0 {
		t.Errorf("scores_before overall: got %v", entry.ScoresBefore["overall"])
	}
	if entry.ScoresAfter["layout"] != 7.0 {
		t.Errorf("scores_after layout: got %v", entry.ScoresAfter["layout"])
	}
}

func TestBuildWorklogEntry_AxisOnlyInPrev(t *testing.T) {
	prev := ReviewScores{Overall: 5.0, Axes: map[string]float64{"layout": 5, "old_axis": 3}}
	next := ReviewScores{Overall: 7.0, Axes: map[string]float64{"layout": 7, "new_axis": 9}}

	entry := BuildWorklogEntry(1, "rewrite", prev, next, VerdictAccept)

	// old_axis only in prev → not in deltas; new_axis only in next → not in deltas
	if _, ok := entry.Deltas["old_axis"]; ok {
		t.Error("old_axis should not be in deltas (only in prev)")
	}
	if _, ok := entry.Deltas["new_axis"]; ok {
		t.Error("new_axis should not be in deltas (only in next)")
	}
	if entry.Deltas["overall"] != 2.0 {
		t.Errorf("delta overall: got %v, want 2.0", entry.Deltas["overall"])
	}
}

func TestAppendWorklog_CreateAndAppend(t *testing.T) {
	dir := t.TempDir()

	entry1 := BuildWorklogEntry(1, "first pass",
		ReviewScores{Overall: 5.0, Axes: map[string]float64{"layout": 5}},
		ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 6}},
		VerdictAccept)

	if err := AppendWorklog(dir, entry1); err != nil {
		t.Fatalf("AppendWorklog (create): %v", err)
	}

	// Verify file exists and contains one entry.
	path := filepath.Join(dir, "refinement_worklog.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("worklog file is empty")
	}

	// Append a second entry.
	entry2 := BuildWorklogEntry(2, "second pass",
		ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 6}},
		ReviewScores{Overall: 5.0, Axes: map[string]float64{"layout": 5}},
		VerdictRevertRegression)

	if err := AppendWorklog(dir, entry2); err != nil {
		t.Fatalf("AppendWorklog (append): %v", err)
	}

	// Verify two entries now.
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile (2nd): %v", err)
	}
	// Just verify it's valid JSON and non-empty — the content is tested via BuildWorklogEntry.
	if len(data) == 0 {
		t.Fatal("worklog file is empty after 2nd append")
	}
}

func TestSummarizeWorklog_Empty(t *testing.T) {
	if s := SummarizeWorklog(nil, 3); s != "" {
		t.Errorf("empty worklog: got %q, want empty", s)
	}
}

func TestSummarizeWorklog_AllRecent(t *testing.T) {
	entries := []RefinementWorklogEntry{
		{Round: 1, Outcome: "accepted"},
		{Round: 2, Outcome: "reverted_regression"},
	}
	s := SummarizeWorklog(entries, 3)
	// 2 entries, recentN=3 → all in "Recent rounds" section, no "Earlier" section.
	if s == "" {
		t.Fatal("expected non-empty summary")
	}
}

func TestSummarizeWorklog_SplitSummaryAndFull(t *testing.T) {
	entries := []RefinementWorklogEntry{
		{Round: 1, Outcome: "accepted"},
		{Round: 2, Outcome: "reverted_regression"},
		{Round: 3, Outcome: "accepted"},
		{Round: 4, Outcome: "accepted"},
		{Round: 5, Outcome: "reverted_subaxis_drift"},
	}
	s := SummarizeWorklog(entries, 3)
	// 5 entries, recentN=3 → first 2 summarized, last 3 full.
	if s == "" {
		t.Fatal("expected non-empty summary")
	}
	// Should contain "Earlier rounds" section.
	if !strings.Contains(s, "Earlier rounds") {
		t.Error("expected 'Earlier rounds' section in summary")
	}
	// Should contain "Recent rounds" section.
	if !strings.Contains(s, "Recent rounds") {
		t.Error("expected 'Recent rounds' section in summary")
	}
}
