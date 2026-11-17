package api

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// maxTasksListRows caps how many tasks /tasks prints. Telegram truncates a
// message at 4096 chars and 20 short rows fit with room for the header.
const maxTasksListRows = 20

// taskTimeLayout is date+time without seconds: enough to read a window within
// one year, short enough to keep a row on a single Telegram line.
const taskTimeLayout = "01-02 15:04"

// formatTasksList renders the /tasks reply from scheduler status dicts.
//
// It replaces two defects in the old inline version:
//   - the row read g["total_steps"], a key ToStatusDict never emits (the step
//     maps live under "steps", schemas/task.go:973), so every task printed
//     "(0 steps)";
//   - rows were printed in ListGraphs' own order, and the in-memory half of
//     that order comes from Go map iteration, so "last 20" kept 20 arbitrary
//     tasks instead of the newest 20.
func formatTasksList(graphs []map[string]any) string {
	if len(graphs) == 0 {
		return "No tasks found."
	}
	sort.SliceStable(graphs, func(i, j int) bool {
		return taskCreatedAt(graphs[i]).After(taskCreatedAt(graphs[j]))
	})
	if len(graphs) > maxTasksListRows {
		graphs = graphs[:maxTasksListRows]
	}
	lines := make([]string, 0, len(graphs))
	for _, g := range graphs {
		id, _ := g["task_id"].(string)
		status, _ := g["status"].(string)
		steps, _ := g["steps"].(map[string]any)
		row := fmt.Sprintf("  %s [%s] (%d steps) %s", id, status, len(steps), formatTaskWindow(g))
		if marker := formatTaskStaleness(g); marker != "" {
			row += " " + marker
		}
		lines = append(lines, row)
	}
	return fmt.Sprintf("Recent tasks (last %d):\n%s", len(graphs), strings.Join(lines, "\n"))
}

// formatTaskStaleness renders the Q4-6/Q5-5 signals as a short row marker. The
// status dict already carries the full error sentence for get_task_status; a
// one-line list only has room for which signal fired and how old it is.
func formatTaskStaleness(g map[string]any) string {
	var markers []string
	if age := staleSignalAge(g, "delegation_wait"); age > 0 {
		markers = append(markers, "delegation unanswered "+compactAge(age))
	}
	if age := staleSignalAge(g, "stale_decisions"); age > 0 {
		markers = append(markers, "step_failed stale "+compactAge(age))
	}
	if len(markers) == 0 {
		return ""
	}
	return "[" + strings.Join(markers, "; ") + "]"
}

// staleSignalAge reads the scalar age the attach helpers write next to each
// report. Numeric types are handled explicitly because a status dict that went
// through JSON (float64) must not silently lose the marker.
func staleSignalAge(g map[string]any, key string) time.Duration {
	report, _ := g[key].(map[string]any)
	if report == nil {
		return 0
	}
	var secs int
	switch v := report["oldest_wait_seconds"].(type) {
	case int:
		secs = v
	case int64:
		secs = int(v)
	case float64:
		secs = int(v)
	}
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// compactAge is compactDuration without the zero sub-unit: "26h", "31m", "45s".
func compactAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// formatTaskWindow renders "start → end (duration)". The end stamp is "-" while
// the task is still running, and the duration only appears once both stamps
// exist. A task with no parseable created_at prints its status as-is rather
// than a fake time.
func formatTaskWindow(g map[string]any) string {
	start := taskCreatedAt(g)
	if start.IsZero() {
		return "start unknown"
	}
	end := taskCompletedAt(g)
	if end.IsZero() {
		return start.Format(taskTimeLayout) + " → -"
	}
	return fmt.Sprintf("%s → %s (%s)",
		start.Format(taskTimeLayout), end.Format(taskTimeLayout), compactDuration(end.Sub(start)))
}

func taskCreatedAt(g map[string]any) time.Time   { return statusTime(g, "created_at") }
func taskCompletedAt(g map[string]any) time.Time { return statusTime(g, "completed_at") }

// statusTime parses an RFC3339 stamp out of a status dict. ToStatusDict writes
// created_at unconditionally (schemas/task.go:978) but completed_at only for a
// finished graph (:997), so a missing key parses as the zero time.
func statusTime(g map[string]any, key string) time.Time {
	s, _ := g[key].(string)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.Local()
}

// compactDuration renders the shortest useful form: "12s", "4m31s", "2h14m".
func compactDuration(d time.Duration) string {
	switch {
	case d < 0:
		// A manifest restored from disk can carry stamps that disagree; a
		// negative window would otherwise print as "-12s".
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
