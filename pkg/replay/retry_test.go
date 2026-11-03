package replay

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Regression test for retries disappearing from the replay. The scheduler
// re-dispatches a step by writing a fresh audit_entry span plus a new pair of
// step_started events; only the HTTP-failure path also writes step_retrying.
// The loader counted retries solely from step_retrying, so a re-dispatched step
// reported retries=0 and the timeline had no self-healing beat at all — the
// replays of task_00c77e41 (three dispatches of s1) and task_177842d2 (two)
// both looked like a single clean attempt.
// Link: one audit_entry span per dispatch, measured across logs/2026-10-29,
// logs/2026-10-31 and logs/2026-11-01.

func retryLoader(t *testing.T, taskID string, now time.Time, lines ...string) *Loader {
	t.Helper()
	root := t.TempDir()
	writeLogFile(t, root, now.Format("2006-01-02"), taskID, lines...)
	loader := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks"))
	loader.now = func() time.Time { return now }
	return loader
}

// dispatch writes the events one attempt produces: the audit span, then the
// interceptor start and the dispatch start that always follow it.
func dispatch(taskID, step string, at float64, span string) []string {
	return []string{
		line(taskID, "audit_entry", step, at, `{"role_id":"reader","span_id":"`+span+`","task":"read the file"}`),
		line(taskID, "step_started", step, at, `{"message":"Interceptor: Optimizing prompt","role":"reader"}`),
		line(taskID, "step_started", step, at+2, `{"mcp_bindings":[],"role":"reader","skills":[]}`),
	}
}

func toolCall(taskID, step string, at float64) string {
	return line(taskID, "EventCoreToolCall", step, at, `{"tool":"read_file","status":"success"}`)
}

func completed(taskID, step string, at float64) string {
	return line(taskID, "step_completed", step, at, `{"status":"ok","message":"done"}`)
}

func failed(taskID, step string, at float64) string {
	return line(taskID, "step_failed", step, at, `{"error":"context canceled","message":"failed"}`)
}

func retrying(taskID, step string, at float64, attempt int) string {
	return line(taskID, "step_retrying", step, at, `{"attempt":`+strconv.Itoa(attempt)+`,"error":"context canceled"}`)
}

func assertRetries(t *testing.T, loader *Loader, taskID string, wantStep, wantRun, wantBeats int) {
	t.Helper()
	detail, err := loader.Detail(taskID)
	if err != nil {
		t.Fatalf("Detail(%s): %v", taskID, err)
	}
	if len(detail.Steps) == 0 {
		t.Fatalf("no steps reconstructed for %s", taskID)
	}
	beats := 0
	for _, event := range detail.Events {
		if event.Kind == KindRetry {
			beats++
		}
	}
	if got := detail.Steps[0].Retries; got != wantStep {
		t.Errorf("step retries = %d, want %d", got, wantStep)
	}
	if got := detail.Retries; got != wantRun {
		t.Errorf("run retries = %d, want %d", got, wantRun)
	}
	if beats != wantBeats {
		t.Errorf("retry beats = %d, want %d", beats, wantBeats)
	}
}

// Link: two dispatches of the same step and no step_retrying anywhere — the
// shape that reported zero retries before the loader knew about audit spans.
func TestRedispatchCountsRetryWithoutStepRetrying(t *testing.T) {
	const taskID = "task_retry001"
	now := statusNow()
	lines := []string{line(taskID, "task_submitted", "", ago(now, 600), `{"step_count":1}`)}
	lines = append(lines, dispatch(taskID, "s1", ago(now, 590), "span-a")...)
	lines = append(lines, toolCall(taskID, "s1", ago(now, 450)))
	lines = append(lines, dispatch(taskID, "s1", ago(now, 300), "span-b")...)
	lines = append(lines, toolCall(taskID, "s1", ago(now, 200)))
	lines = append(lines, completed(taskID, "s1", ago(now, 100)))
	lines = append(lines, line(taskID, "task_completed", "", ago(now, 90), `{}`))

	assertRetries(t, retryLoader(t, taskID, now, lines...), taskID, 1, 1, 1)
}

// Link: the HTTP-failure path logs step_retrying and then re-dispatches. Both
// signals describe one retry, so it must count and animate once.
func TestStepRetryingFollowedByFreshSpanCountsOnce(t *testing.T) {
	const taskID = "task_retry002"
	now := statusNow()
	lines := []string{line(taskID, "task_submitted", "", ago(now, 600), `{"step_count":1}`)}
	lines = append(lines, dispatch(taskID, "s1", ago(now, 590), "span-a")...)
	lines = append(lines, toolCall(taskID, "s1", ago(now, 450)))
	lines = append(lines, failed(taskID, "s1", ago(now, 300)))
	lines = append(lines, retrying(taskID, "s1", ago(now, 299), 2))
	lines = append(lines, dispatch(taskID, "s1", ago(now, 298), "span-b")...)
	lines = append(lines, completed(taskID, "s1", ago(now, 100)))

	assertRetries(t, retryLoader(t, taskID, now, lines...), taskID, 1, 1, 1)
}

// Link: a logged retry that never got a second dispatch is still one retry —
// the case that must not regress to len(spans)-1 arithmetic.
func TestStepRetryingWithoutSecondDispatchStillCountsRetry(t *testing.T) {
	const taskID = "task_retry003"
	now := statusNow()
	lines := []string{line(taskID, "task_submitted", "", ago(now, 600), `{"step_count":1}`)}
	lines = append(lines, dispatch(taskID, "s1", ago(now, 590), "span-a")...)
	lines = append(lines, failed(taskID, "s1", ago(now, 300)))
	lines = append(lines, retrying(taskID, "s1", ago(now, 299), 2))

	assertRetries(t, retryLoader(t, taskID, now, lines...), taskID, 1, 1, 1)
}

// Three dispatches of one step carry two retries, and each one gets a beat.
func TestThreeDispatchesCarryTwoRetries(t *testing.T) {
	const taskID = "task_retry004"
	now := statusNow()
	lines := []string{line(taskID, "task_submitted", "", ago(now, 900), `{"step_count":1}`)}
	lines = append(lines, dispatch(taskID, "s1", ago(now, 890), "span-a")...)
	lines = append(lines, dispatch(taskID, "s1", ago(now, 600), "span-b")...)
	lines = append(lines, dispatch(taskID, "s1", ago(now, 300), "span-c")...)
	lines = append(lines, completed(taskID, "s1", ago(now, 100)))

	assertRetries(t, retryLoader(t, taskID, now, lines...), taskID, 2, 2, 2)
}

// lateDispatch is an attempt whose step_started lands 55 s after its audit span,
// the spacing measured on task_177842d2.
func lateDispatch(taskID, step string, at float64, span string) []string {
	return []string{
		line(taskID, "audit_entry", step, at, `{"role_id":"reader","span_id":"`+span+`","task":"read the file"}`),
		line(taskID, "step_started", step, at+55, `{"message":"Interceptor: Optimizing prompt","role":"reader"}`),
		line(taskID, "step_started", step, at+55.2, `{"mcp_bindings":[],"role":"reader","skills":[]}`),
	}
}

// Link: the beat was once found by matching the audit span against a
// step_started a few seconds away. In the real logs the re-dispatch's first
// step_started landed 55 s after its span, so the retry counted in the tally
// while the timeline stayed silent — the failed run replayed as if it had never
// tried again.
func TestRetryBeatSurvivesLateDispatchStart(t *testing.T) {
	const taskID = "task_retry005"
	now := statusNow()
	lines := []string{line(taskID, "task_submitted", "", ago(now, 900), `{"step_count":1}`)}
	lines = append(lines, dispatch(taskID, "s1", ago(now, 890), "span-a")...)
	lines = append(lines, failed(taskID, "s1", ago(now, 500)))
	lines = append(lines, lateDispatch(taskID, "s1", ago(now, 300), "span-b")...)
	lines = append(lines, completed(taskID, "s1", ago(now, 100)))

	loader := retryLoader(t, taskID, now, lines...)
	assertRetries(t, loader, taskID, 1, 1, 1)

	detail, err := loader.Detail(taskID)
	if err != nil {
		t.Fatalf("Detail(%s): %v", taskID, err)
	}
	var beatAt int64 = -1
	for _, event := range detail.Events {
		if event.Kind == KindRetry {
			beatAt = event.AtMs
		}
	}
	// The beat is stamped from the dispatch itself, so it stays on the replay
	// clock even when the first line of the attempt is a minute late.
	if want := int64(600000); beatAt != want {
		t.Errorf("retry beat at %d ms, want %d", beatAt, want)
	}
}
