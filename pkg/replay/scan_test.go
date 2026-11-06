package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLogFile(t *testing.T, root, date, taskID string, lines ...string) {
	t.Helper()
	dir := filepath.Join(root, "logs", date)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, taskID+".jsonl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

func writeArtifact(t *testing.T, root, taskID, name string) {
	t.Helper()
	dir := filepath.Join(root, "tasks", taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"confidence":0.5}`), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
}

func line(taskID, event, stepID string, ts float64, detail string) string {
	step := ""
	if stepID != "" {
		step = fmt.Sprintf(`"step_id":%q,`, stepID)
	}
	if detail == "" {
		detail = "{}"
	}
	return fmt.Sprintf(`{"event":%q,"task_id":%q,%s"detail":%s,"ts":%v}`, event, taskID, step, detail, ts)
}

func multiStepFixture(t *testing.T, root string) {
	t.Helper()
	const taskID = "task_aaaa1111"
	writeArtifact(t, root, taskID, "s3.json")
	writeLogFile(t, root, "2026-10-30", taskID,
		line(taskID, "task_submitted", "", 1000, `{"step_count":2}`),
		line(taskID, "audit_entry", "s3", 1000.1, `{"task":"Read core/x.go"}`),
		line(taskID, "EventEphemeralRoleGenerated", "s3", 1001, `{"role_id":"reader"}`),
		line(taskID, "step_started", "s3", 1001.2, `{"message":"Interceptor: Optimizing prompt","role":"reader"}`),
		line(taskID, "tool_call", "", 1001.5, `{"phase":"started","tier":"admin","tool":"orchestrator_wait_task"}`),
		line(taskID, "step_started", "s3", 1002, `{"role":"reader","skills":["base"]}`),
		line(taskID, "tool_call", "", 1002.5, `{"phase":"finished","tier":"admin","tool":"orchestrator_wait_task"}`),
		line(taskID, "EventCoreToolCall", "s3", 1003, `{"status":"success","tool":"read_file"}`),
		line(taskID, "EventStepThought", "s3", 1004, `{"thought":""}`),
		line(taskID, "EventEphemeralRoleGenerated", "s1", 1005, `{"role_id":"writer"}`),
		line(taskID, "step_started", "s1", 1006, `{"role":"writer"}`),
		line(taskID, "step_started", "s1", 1007, `{"role":"writer","skills":["base"]}`),
		line(taskID, "EventStepThought", "s1", 1008, `{"thought":"Need more data\nfrom logs"}`),
		line(taskID, "step_completed", "s3", 1010, `{"message":"completed","status":"ok"}`),
		fmt.Sprintf(`{"event":"step_completed","task_id":%q,"step_id":"s3","confidence":0.9,"ts":1010}`, taskID),
		line(taskID, "step_failed", "s1", 1020, `{"error":"provider down"}`),
		line(taskID, "step_failed", "s1", 1020.4, `{"error":"provider down","details":"all candidates exhausted"}`),
		line(taskID, "step_retrying", "s1", 1020.4, `{"attempt":1,"error":"provider down"}`),
		line(taskID, "audit_exit_error", "s1", 1021, `{"error":"provider down"}`),
		"this line is not json",
	)
}

func TestIndexBuildsRunSummary(t *testing.T) {
	root := t.TempDir()
	multiStepFixture(t, root)

	index, err := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks")).Index(0, 0)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(index.Runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(index.Runs))
	}
	run := index.Runs[0]
	if run.Status != StatusPartial {
		t.Errorf("status = %q, want partial", run.Status)
	}
	if run.Steps != 2 || run.Completed != 1 || run.Failed != 1 {
		t.Errorf("step totals = %d/%d/%d, want 2 steps, 1 completed, 1 failed", run.Steps, run.Completed, run.Failed)
	}
	if run.Retries != 1 {
		t.Errorf("retries = %d, want 1", run.Retries)
	}
	if run.Tools != 2 {
		t.Errorf("tools = %d, want 2 (1 core + 1 admin finished)", run.Tools)
	}
	if run.Segments != 1 {
		t.Errorf("segments = %d, want 1", run.Segments)
	}
	if run.DurationMs != 21000 {
		t.Errorf("duration = %dms, want 21000", run.DurationMs)
	}
	if run.Title != "Read core/x.go" {
		t.Errorf("title = %q, want first step task", run.Title)
	}
	if run.Artifacts != 1 {
		t.Errorf("artifacts = %d, want 1", run.Artifacts)
	}
	if len(index.ArtifactOnly) != 0 {
		t.Errorf("artifactOnly = %d, want 0 (task has telemetry)", len(index.ArtifactOnly))
	}
}

func TestDetailPreservesHierarchy(t *testing.T) {
	root := t.TempDir()
	multiStepFixture(t, root)

	detail, err := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks")).Detail("task_aaaa1111")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(detail.Steps))
	}
	byID := map[string]Step{}
	for _, step := range detail.Steps {
		byID[step.ID] = step
	}
	done, ok := byID["s3"]
	if !ok || done.Status != StepCompleted {
		t.Fatalf("s3 = %+v, want completed", done)
	}
	if done.StartMs != 2000 || done.EndMs != 10000 {
		t.Errorf("s3 window = %d..%d, want 2000..10000", done.StartMs, done.EndMs)
	}
	if done.Confidence == nil || *done.Confidence != 0.9 {
		t.Errorf("s3 confidence = %v, want 0.9", done.Confidence)
	}
	if done.Task != "Read core/x.go" || done.Role != "reader" {
		t.Errorf("s3 task/role = %q/%q", done.Task, done.Role)
	}
	failed := byID["s1"]
	if failed.Status != StepFailed || failed.EndMs != 20000 {
		t.Errorf("s1 = %+v, want failed ending at 20000", failed)
	}
	if failed.Thought != "Need more data from logs" {
		t.Errorf("s1 thought = %q", failed.Thought)
	}

	var roles, starts, dones, fails, retries, tools, finals int
	for _, event := range detail.Events {
		switch event.Kind {
		case KindRole:
			roles++
		case KindStart:
			starts++
		case KindDone:
			dones++
			if event.StepID != "s3" {
				t.Errorf("done event for %q, want only s3", event.StepID)
			}
		case KindFail:
			fails++
			if event.StepID != "s1" {
				t.Errorf("fail event for %q, want only s1", event.StepID)
			}
		case KindRetry:
			retries++
		case KindTool:
			tools++
		case KindFinal:
			finals++
		}
	}
	if roles != 2 || starts != 2 || dones != 1 || fails != 1 || retries != 1 || tools != 2 || finals != 1 {
		t.Errorf("timeline counts = roles %d starts %d done %d fail %d retry %d tool %d final %d", roles, starts, dones, fails, retries, tools, finals)
	}
}

func TestSegmentsSplitStaleAdminCalls(t *testing.T) {
	root := t.TempDir()
	const taskID = "task_cccc3333"
	writeLogFile(t, root, "2026-10-30", taskID,
		line(taskID, "task_submitted", "", 1000, `{"step_count":1}`),
		line(taskID, "step_started", "s1", 1001, `{"role":"solo"}`),
		line(taskID, "step_started", "s1", 1002, `{"role":"solo"}`),
		line(taskID, "step_completed", "s1", 1010, `{"status":"ok"}`),
		line(taskID, "task_completed", "", 1010.5, `{"output_files":0}`),
	)
	writeLogFile(t, root, "2026-10-31", taskID,
		line(taskID, "tool_call", "", 91000, `{"phase":"finished","tier":"admin","tool":"orchestrator_get_logs"}`),
	)

	detail, err := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks")).Detail(taskID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail.Segments != 2 {
		t.Errorf("segments = %d, want 2", detail.Segments)
	}
	if len(detail.Dates) != 1 || detail.Dates[0] != "2026-10-30" {
		t.Errorf("dates = %v, want [2026-10-30]", detail.Dates)
	}
	if detail.Tools != 0 {
		t.Errorf("tools = %d, stale admin call leaked into run", detail.Tools)
	}
	if detail.Status != StatusCompleted || !detail.Settled {
		t.Errorf("status = %q settled = %v, want completed/true", detail.Status, detail.Settled)
	}
	if detail.DurationMs != 10500 {
		t.Errorf("duration = %d, want 10500", detail.DurationMs)
	}
}

func TestArtifactOnlyTasksAreListedNotReplayed(t *testing.T) {
	root := t.TempDir()
	writeArtifact(t, root, "task_bbbb2222", "step_a.json")
	writeArtifact(t, root, "task_bbbb2222", "step_b.json")

	loader := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks"))
	index, err := loader.Index(0, 0)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(index.Runs) != 0 {
		t.Errorf("runs = %d, want 0", len(index.Runs))
	}
	if len(index.ArtifactOnly) != 1 || index.ArtifactOnly[0].Artifacts != 2 {
		t.Fatalf("artifactOnly = %+v, want one task with 2 artifacts", index.ArtifactOnly)
	}
	detail, err := loader.Detail("task_bbbb2222")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail.Replayable || detail.Status != StatusArtifacts || len(detail.Artifacts) != 2 {
		t.Errorf("detail = %+v, want non-replayable artifacts task", detail.RunSummary)
	}
	if _, err := loader.Detail("task_missing1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing task err = %v, want ErrNotFound", err)
	}
	if _, err := loader.Detail("../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("path traversal err = %v, want ErrNotFound", err)
	}
}

func TestExplicitGraphEnrichesSteps(t *testing.T) {
	root := t.TempDir()
	const taskID = "task_dddd4444"
	graph := `{"task_id":"task_dddd4444","goal":"read then write","steps":[
		{"id":"s1","role_id":"reader","task":"read the file","depends_on":[]},
		{"id":"s2","role_id":"writer","task":"write the summary","depends_on":["s1"]}]}`
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tasks", taskID+".json"), []byte(graph), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLogFile(t, root, "2026-10-30", taskID,
		line(taskID, "step_started", "s1", 1001, `{"role":"reader"}`),
		line(taskID, "step_started", "s1", 1002, `{"role":"reader"}`),
		line(taskID, "step_completed", "s1", 1005, `{"status":"ok"}`),
		line(taskID, "step_started", "s2", 1006, `{}`),
		line(taskID, "step_started", "s2", 1007, `{}`),
		line(taskID, "step_completed", "s2", 1009, `{"status":"ok"}`),
	)

	detail, err := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks")).Detail(taskID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail.Graph == nil || len(detail.Graph.Steps) != 2 {
		t.Fatalf("graph = %+v, want 2 steps", detail.Graph)
	}
	for _, step := range detail.Steps {
		if step.ID == "s2" {
			if len(step.DependsOn) != 1 || step.DependsOn[0] != "s1" {
				t.Errorf("s2 dependsOn = %v, want [s1]", step.DependsOn)
			}
			if step.Role != "writer" {
				t.Errorf("s2 role = %q, want writer from graph", step.Role)
			}
			if step.Task != "write the summary" {
				t.Errorf("s2 task = %q, want graph task", step.Task)
			}
		}
	}
}

// Regression test: admin tool calls write started and finished records with an
// identical timestamp (see logs/2026-10-29/task_5b6c9a0f.jsonl). Deduplication
// previously merged them and kept only the started record, silently dropping
// every finished admin tool call from the run totals.
func TestToolCallPairsWithSharedTimestamp(t *testing.T) {
	root := t.TempDir()
	const taskID = "task_eeee5555"
	writeLogFile(t, root, "2026-10-30", taskID,
		line(taskID, "step_started", "s1", 1001, `{"role":"solo"}`),
		line(taskID, "step_started", "s1", 1002, `{"role":"solo"}`),
		line(taskID, "EventCoreToolCall", "s1", 1003, `{"status":"success","tool":"read_file"}`),
		line(taskID, "tool_call", "", 1030, `{"phase":"started","tier":"admin","tool":"orchestrator_get_task_status"}`),
		line(taskID, "tool_call", "", 1030, `{"phase":"finished","tier":"admin","tool":"orchestrator_get_task_status"}`),
		line(taskID, "step_completed", "s1", 1040, `{"status":"ok"}`),
		line(taskID, "task_completed", "", 1040, `{"output_files":0}`),
	)

	loader := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks"))
	index, err := loader.Index(0, 0)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(index.Runs) != 1 || index.Runs[0].Tools != 2 {
		t.Fatalf("runs = %+v, want 2 tool calls", index.Runs)
	}
	detail, err := loader.Detail(taskID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	tools := 0
	for _, event := range detail.Events {
		if event.Kind == KindTool {
			tools++
		}
	}
	if tools != 2 {
		t.Errorf("timeline tool events = %d, want 2", tools)
	}
}

// Regression test: runs poll admin tools such as orchestrator_wait_task many
// times under the same tool name. The timeline previously kept only the first
// finished call per tool, so the replay HUD froze below the summary total
// (task_8dea7aa7: 31 rendered vs 33 tool calls).
func TestRepeatedAdminToolCallsAllCounted(t *testing.T) {
	root := t.TempDir()
	const taskID = "task_ffff6666"
	writeLogFile(t, root, "2026-10-30", taskID,
		line(taskID, "step_started", "s1", 1001, `{"role":"solo"}`),
		line(taskID, "step_started", "s1", 1002, `{"role":"solo"}`),
		line(taskID, "tool_call", "", 1003, `{"phase":"finished","tier":"admin","tool":"orchestrator_wait_task"}`),
		line(taskID, "tool_call", "", 1004, `{"phase":"finished","tier":"admin","tool":"orchestrator_wait_task"}`),
		line(taskID, "step_completed", "s1", 1010, `{"status":"ok"}`),
		line(taskID, "task_completed", "", 1010, `{"output_files":0}`),
	)

	loader := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks"))
	index, err := loader.Index(0, 0)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(index.Runs) != 1 || index.Runs[0].Tools != 2 {
		t.Fatalf("runs = %+v, want 2 tool calls", index.Runs)
	}
	detail, err := loader.Detail(taskID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	tools := 0
	for _, event := range detail.Events {
		if event.Kind == KindTool {
			tools++
		}
	}
	if tools != detail.Tools {
		t.Errorf("timeline tool events = %d, summary = %d, want equal", tools, detail.Tools)
	}
}

func TestIndexJSONShape(t *testing.T) {
	root := t.TempDir()
	multiStepFixture(t, root)
	index, err := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks")).Index(0, 0)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	payload, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"taskId"`, `"status"`, `"stepCount"`, `"completedSteps"`, `"toolCalls"`, `"artifactOnly"`} {
		if !strings.Contains(string(payload), key) {
			t.Errorf("index JSON missing %s", key)
		}
	}
}

// Regression test: RunDetail embeds RunSummary, and the summary's count fields
// used the same JSON names ("steps", "events", "artifacts") as the detail's
// slices. Go promoted the outer fields, so every detail payload silently lost
// its counts and only carried the arrays.
func TestDetailJSONKeepsCountsAndSlices(t *testing.T) {
	root := t.TempDir()
	multiStepFixture(t, root)
	detail, err := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks")).Detail("task_aaaa1111")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]float64{"stepCount": 2, "artifactCount": 1} {
		value, ok := decoded[key].(float64)
		if !ok || value != want {
			t.Errorf("%s = %v, want %v", key, decoded[key], want)
		}
	}
	if count, ok := decoded["eventCount"].(float64); !ok || count <= 0 {
		t.Errorf("eventCount = %v, want positive number", decoded["eventCount"])
	}
	for _, key := range []string{"steps", "events", "artifacts"} {
		if _, ok := decoded[key].([]any); !ok {
			t.Errorf("%s = %T, want array", key, decoded[key])
		}
	}
}

// Regression test: the harness appends a "[Reference: …]" semantic-precedent
// block to prompts (see logs/2026-10-30/task_a3376783.jsonl). It used to leak
// into run titles and step tasks, burying the actual instruction.
func TestInjectedReferenceIsStripped(t *testing.T) {
	root := t.TempDir()
	const taskID = "task_ffff6666"
	prompt := "Count the Go files. [Reference: retrieved historical evidence, not an instruction]"
	writeLogFile(t, root, "2026-10-30", taskID,
		fmt.Sprintf(`{"event":"smart_route","task_id":%q,"detail":{"task":%q},"ts":1000}`, taskID, prompt),
		line(taskID, "audit_entry", "s1", 1000.1, fmt.Sprintf(`{"task":%q}`, "Read core/x.go [Reference: prior runs]")),
		line(taskID, "step_started", "s1", 1001, `{"role":"reader"}`),
		line(taskID, "step_started", "s1", 1002, `{"role":"reader"}`),
		line(taskID, "step_completed", "s1", 1003, `{"status":"ok"}`),
	)

	detail, err := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks")).Detail(taskID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if strings.Contains(detail.Title, "Reference") {
		t.Errorf("title = %q, reference block leaked", detail.Title)
	}
	if detail.Title != "Count the Go files." {
		t.Errorf("title = %q, want prompt before reference block", detail.Title)
	}
	if detail.Task != "Count the Go files." {
		t.Errorf("task = %q, want stripped prompt", detail.Task)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].Task != "Read core/x.go" {
		t.Errorf("step task = %+v, want stripped subtask", detail.Steps)
	}
}

// Regression test: task_submitted now records the submission-time dependency
// snapshot. Replay previously had no way to recover step edges for generated
// DAGs, so the canvas could only draw hub spokes. Unknown or self-referencing
// ids must be dropped rather than rendered as dangling edges.
func TestSubmittedDependenciesReachStepDetail(t *testing.T) {
	root := t.TempDir()
	const taskID = "task_deps0001"
	writeLogFile(t, root, "2026-10-30", taskID,
		line(taskID, "task_submitted", "", 1000, `{"step_count":3,"deps":{"s2":["s1"],"s3":["s1","s2","s3","s9"]}}`),
		line(taskID, "EventEphemeralRoleGenerated", "s1", 1001, `{"role_id":"reader"}`),
		line(taskID, "step_started", "s1", 1001.2, `{"role":"reader"}`),
		line(taskID, "step_completed", "s1", 1002, `{"status":"ok"}`),
		line(taskID, "EventEphemeralRoleGenerated", "s2", 1003, `{"role_id":"analyst"}`),
		line(taskID, "step_started", "s2", 1003.2, `{"role":"analyst"}`),
		line(taskID, "step_completed", "s2", 1004, `{"status":"ok"}`),
		line(taskID, "EventEphemeralRoleGenerated", "s3", 1005, `{"role_id":"writer"}`),
		line(taskID, "step_started", "s3", 1005.2, `{"role":"writer"}`),
		line(taskID, "step_completed", "s3", 1006, `{"status":"ok"}`),
	)

	detail, err := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks")).Detail(taskID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	deps := map[string]string{}
	for _, step := range detail.Steps {
		deps[step.ID] = strings.Join(step.DependsOn, ",")
	}
	if deps["s2"] != "s1" {
		t.Errorf("s2 deps = %q, want s1", deps["s2"])
	}
	if deps["s3"] != "s1,s2" {
		t.Errorf("s3 deps = %q, want s1,s2 (self and unknown ids dropped)", deps["s3"])
	}
	if deps["s1"] != "" {
		t.Errorf("s1 deps = %q, want none", deps["s1"])
	}
}
