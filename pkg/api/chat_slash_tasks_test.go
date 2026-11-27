package api

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// statusRow builds one ListGraphs-shaped dict. Times are RFC3339 because that
// is what ToStatusDict emits (schemas/task.go:978,997).
func statusRow(id, status string, created, completed time.Time, steps int) map[string]any {
	row := map[string]any{
		"task_id":    id,
		"status":     status,
		"created_at": created.Format(time.RFC3339),
	}
	if steps > 0 {
		m := make(map[string]any, steps)
		for i := 0; i < steps; i++ {
			m[fmt.Sprintf("s%d", i)] = map[string]any{"id": fmt.Sprintf("s%d", i)}
		}
		row["steps"] = m
	}
	if !completed.IsZero() {
		row["completed_at"] = completed.Format(time.RFC3339)
	}
	return row
}

// TestFormatTasksList_ShowsStartAndEndTime is the regression for the operator
// request of 2026-11-04: /tasks printed only id/status/steps, so there was no
// way to tell when a task ran.
func TestFormatTasksList_ShowsStartAndEndTime(t *testing.T) {
	start := time.Date(2026, 11, 4, 9, 12, 0, 0, time.Local)
	end := start.Add(19 * time.Minute)
	out := formatTasksList([]map[string]any{statusRow("task_ab12cd34", "completed", start, end, 5)})

	wantStart := start.Format(taskTimeLayout)
	wantEnd := end.Format(taskTimeLayout)
	for _, want := range []string{wantStart, wantEnd, "(19m0s)", "task_ab12cd34", "[completed]", "(5 steps)"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected /tasks line to contain %q, got:\n%s", want, out)
		}
	}
}

// TestFormatTasksList_RunningTaskHasNoEndStamp: completed_at is absent while a
// task runs, and the row must say so instead of printing the zero time.
func TestFormatTasksList_RunningTaskHasNoEndStamp(t *testing.T) {
	start := time.Date(2026, 11, 4, 10, 0, 0, 0, time.Local)
	out := formatTasksList([]map[string]any{statusRow("task_run1", "running", start, time.Time{}, 2)})
	if !strings.Contains(out, start.Format(taskTimeLayout)+" → -") {
		t.Errorf("expected an open-ended window, got:\n%s", out)
	}
	if strings.Contains(out, "0001") {
		t.Errorf("zero time leaked into the output:\n%s", out)
	}
}

// TestFormatTasksList_CountsStepsFromStepsKey is the second defect fixed here:
// the old row read g["total_steps"], a key ToStatusDict never emits, so every
// task displayed "(0 steps)".
func TestFormatTasksList_CountsStepsFromStepsKey(t *testing.T) {
	out := formatTasksList([]map[string]any{
		statusRow("task_steps3", "completed", time.Now().Add(-time.Hour), time.Now(), 3),
	})
	if !strings.Contains(out, "(3 steps)") {
		t.Errorf("expected the step count read from the steps map, got:\n%s", out)
	}
	if strings.Contains(out, "(0 steps)") {
		t.Errorf("total_steps bug still present:\n%s", out)
	}
}

// TestFormatTasksList_NewestFirstAndCapped: ListGraphs returns in-memory
// graphs in Go map iteration order, so the old slice-from-the-tail could keep
// any 20 tasks. /tasks must show the 20 newest, newest first.
func TestFormatTasksList_NewestFirstAndCapped(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	// Deliberately out of order: index 0 holds the newest task.
	order := []int{24, 0, 23, 5, 22, 1, 21, 2, 20, 3, 19, 4, 18, 6, 17, 7, 16, 8, 15, 9, 14, 10, 13, 11, 12}
	graphs := make([]map[string]any, 0, len(order))
	for _, i := range order {
		graphs = append(graphs, statusRow(fmt.Sprintf("task_%02d", i), "completed",
			base.Add(time.Duration(i)*time.Hour), base.Add(time.Duration(i)*time.Hour+time.Minute), 1))
	}

	out := formatTasksList(graphs)
	lines := strings.Split(out, "\n")
	if len(lines) != 21 { // header + 20 rows
		t.Fatalf("expected header + 20 rows, got %d lines:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[1], "  task_24 ") {
		t.Errorf("expected the newest task first, got %q", lines[1])
	}
	if !strings.HasPrefix(lines[20], "  task_05 ") {
		t.Errorf("expected the cutoff at the 5-newest, got %q", lines[20])
	}
	for _, l := range lines[1:] {
		if strings.Contains(l, "task_04") || strings.Contains(l, "task_00") {
			t.Errorf("an older task survived the cap:\n%s", l)
		}
	}
}

func TestFormatTasksList_Empty(t *testing.T) {
	if got := formatTasksList(nil); got != "No tasks found." {
		t.Errorf("expected the empty notice, got %q", got)
	}
}

func TestCompactDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{4*time.Minute + 31*time.Second, "4m31s"},
		{2*time.Hour + 14*time.Minute, "2h14m"},
		{-3 * time.Second, "0s"}, // a manifest whose stamps disagree
	}
	for _, c := range cases {
		if got := compactDuration(c.in); got != c.want {
			t.Errorf("compactDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFormatTasksList_ShowsStalenessMarkers is the Q5-5 visibility half in
// /tasks: the list must say when a delegation has gone unanswered (Q4-6) or a
// failed step's decision has been parked for a day (Q5-5). The full error text
// stays in get_task_status; a one-line row only carries the signal and age.
func TestFormatTasksList_ShowsStalenessMarkers(t *testing.T) {
	start := time.Date(2026, 11, 6, 9, 0, 0, 0, time.Local)
	row := statusRow("task_stale1", "running", start, time.Time{}, 2)
	row["stale_decisions"] = map[string]any{
		"stale": true, "timeout_seconds": 86400, "oldest_wait_seconds": 26 * 3600,
	}
	out := formatTasksList([]map[string]any{row})
	if !strings.Contains(out, "[step_failed stale 26h]") {
		t.Errorf("expected the stale step_failed marker, got:\n%s", out)
	}
}

func TestFormatTaskStaleness(t *testing.T) {
	delegation := map[string]any{"delegation_wait": map[string]any{"oldest_wait_seconds": 31 * 60}}
	failure := map[string]any{"stale_decisions": map[string]any{"oldest_wait_seconds": 26 * 3600}}
	both := map[string]any{}
	for k, v := range delegation {
		both[k] = v
	}
	for k, v := range failure {
		both[k] = v
	}

	cases := []struct {
		name string
		row  map[string]any
		want string
	}{
		{"delegation_only", delegation, "[delegation unanswered 31m]"},
		{"step_failed_only", failure, "[step_failed stale 26h]"},
		{"both", both, "[delegation unanswered 31m; step_failed stale 26h]"},
		{"none", map[string]any{"status": "running"}, ""},
		// A status dict that crossed JSON carries float64, not int.
		{"float64_age", map[string]any{"stale_decisions": map[string]any{"oldest_wait_seconds": float64(90)}}, "[step_failed stale 1m]"},
		// A report without the scalar (older dict shape) must not panic or lie.
		{"report_without_scalar", map[string]any{"delegation_wait": map[string]any{"stale": true}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatTaskStaleness(c.row); got != c.want {
				t.Errorf("formatTaskStaleness(%v) = %q, want %q", c.row, got, c.want)
			}
		})
	}
}
