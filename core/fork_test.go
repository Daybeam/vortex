package core

import (
	"os"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestForkTask_NotFound(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	_, err := s.ForkTask("nonexistent", "step1")
	if err == nil {
		t.Errorf("expected error for nonexistent source task")
	}
}

func TestForkTask_StepNotFound(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "src-1",
		Steps:  map[string]*schemas.Step{"s1": {ID: "s1", Status: schemas.StepOK}},
		Status: schemas.GraphRunning,
	}
	s.Mu.Lock()
	s.graphs["src-1"] = graph
	s.Mu.Unlock()

	_, err := s.ForkTask("src-1", "nonexistent_step")
	if err == nil {
		t.Errorf("expected error for nonexistent step")
	}
}

func TestForkTask_CreatesNewGraphWithLineage(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	// Regression guard: ForkTask must derive its run context from lifecycleCtx,
	// not context.Background(). Before that fix, Stop() hung forever draining
	// the forked run goroutine (which polls a blocked graph indefinitely).
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "src-1",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepOK, DependsOn: nil},
			"s2": {ID: "s2", Status: schemas.StepOK, DependsOn: []string{"s1"}},
			"s3": {ID: "s3", Status: schemas.StepPending, DependsOn: []string{"s2"}},
		},
		Status:    schemas.GraphRunning,
		CreatedAt: time.Now(),
	}
	s.Mu.Lock()
	s.graphs["src-1"] = graph
	s.Mu.Unlock()

	newTaskID, err := s.ForkTask("src-1", "s1")
	if err != nil {
		t.Fatalf("ForkTask failed: %v", err)
	}

	s.Mu.RLock()
	forked := s.graphs[newTaskID]
	s.Mu.RUnlock()

	if forked == nil {
		t.Fatalf("forked graph not found")
	}
	if forked.ParentTaskID != "src-1" {
		t.Errorf("expected ParentTaskID=src-1, got %s", forked.ParentTaskID)
	}
	if forked.ForkedAtStep != "s1" {
		t.Errorf("expected ForkedAtStep=s1, got %s", forked.ForkedAtStep)
	}
	if forked.TaskID == "src-1" {
		t.Errorf("forked task should have a new ID, not same as source")
	}
}

func TestForkTask_DownstreamStepsReset(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "src-2",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepOK},
			"s2": {ID: "s2", Status: schemas.StepOK, DependsOn: []string{"s1"}, RetryCount: 3, LastError: "old error"},
			"s3": {ID: "s3", Status: schemas.StepOK, DependsOn: []string{"s2"}},
		},
		Status:    schemas.GraphRunning,
		CreatedAt: time.Now(),
	}
	s.Mu.Lock()
	s.graphs["src-2"] = graph
	s.Mu.Unlock()

	newTaskID, err := s.ForkTask("src-2", "s1")
	if err != nil {
		t.Fatalf("ForkTask failed: %v", err)
	}

	s.Mu.RLock()
	forked := s.graphs[newTaskID]
	s.Mu.RUnlock()

	// s1 should be OK (fork point)
	if forked.Steps["s1"].Status != schemas.StepOK {
		t.Errorf("expected s1=StepOK, got %v", forked.Steps["s1"].Status)
	}
	// s2 depends on s1, should be reset to Pending
	if forked.Steps["s2"].Status != schemas.StepPending {
		t.Errorf("expected s2=StepPending, got %v", forked.Steps["s2"].Status)
	}
	if forked.Steps["s2"].RetryCount != 0 {
		t.Errorf("expected s2 RetryCount=0, got %d", forked.Steps["s2"].RetryCount)
	}
	// s3 depends on s2 (transitive), should also be reset
	if forked.Steps["s3"].Status != schemas.StepPending {
		t.Errorf("expected s3=StepPending, got %v", forked.Steps["s3"].Status)
	}
}

func TestIsDownstream(t *testing.T) {
	steps := map[string]*schemas.Step{
		"a": {ID: "a", DependsOn: nil},
		"b": {ID: "b", DependsOn: []string{"a"}},
		"c": {ID: "c", DependsOn: []string{"b"}},
		"d": {ID: "d", DependsOn: []string{"a"}},
		"e": {ID: "e", DependsOn: nil},
	}
	if !isDownstream("b", "a", steps) {
		t.Errorf("b should be downstream of a")
	}
	if !isDownstream("c", "a", steps) {
		t.Errorf("c should be downstream of a (transitive)")
	}
	if !isDownstream("d", "a", steps) {
		t.Errorf("d should be downstream of a")
	}
	if isDownstream("e", "a", steps) {
		t.Errorf("e should NOT be downstream of a")
	}
	if isDownstream("a", "a", steps) {
		t.Errorf("a should NOT be downstream of itself")
	}
}

func TestCopyDir(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir() + "-copy"

	os.WriteFile(src+"/file1.txt", []byte("hello"), 0o644)
	os.MkdirAll(src+"/sub", 0o755)
	os.WriteFile(src+"/sub/file2.txt", []byte("world"), 0o644)

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir failed: %v", err)
	}

	data, err := os.ReadFile(dst + "/file1.txt")
	if err != nil || string(data) != "hello" {
		t.Errorf("file1 not copied correctly")
	}
	data, err = os.ReadFile(dst + "/sub/file2.txt")
	if err != nil || string(data) != "world" {
		t.Errorf("sub/file2 not copied correctly")
	}
}

func TestLogger_SubscribeEvents(t *testing.T) {
	logDir := mkdirTemp(t)
	logger, _ := NewLogger(logDir, &config.SystemSettings{})
	defer logger.Close()

	ch := logger.SubscribeEvents()
	defer logger.UnsubscribeEvents(ch)

	logger.Log("test_event", "task-1", "step-1", map[string]any{"key": "val"})

	select {
	case evt := <-ch:
		if evt.TaskID != "task-1" {
			t.Errorf("expected task-1, got %s", evt.TaskID)
		}
		if evt.Event != "test_event" {
			t.Errorf("expected test_event, got %s", evt.Event)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("expected to receive event on subscriber channel")
	}
}

func TestLogger_UnsubscribeEvents(t *testing.T) {
	logDir := mkdirTemp(t)
	logger, _ := NewLogger(logDir, &config.SystemSettings{})
	defer logger.Close()

	ch := logger.SubscribeEvents()
	logger.UnsubscribeEvents(ch)

	_, ok := <-ch
	if ok {
		t.Errorf("expected channel to be closed after unsubscribe")
	}
}
