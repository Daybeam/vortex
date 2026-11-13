package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// TestTaskProgressHash_StableForSameState verifies that the same graph
// state produces the same hash, and a different state produces a
// different hash.
//
// The stability half is a loop, not a single comparison, because the bug it
// guards was Go's randomised map iteration: one comparison caught it only half
// the time on a 2-step graph (measured 2 failures in 8 runs at HEAD). 64 rounds
// make a pre-fix pass probability 2^-63, so this is a reliable regression rather
// than a coin flip. Bug: F12 in
// docs/plan/TELEGRAM_CALLBACK_AND_VCP_FINDINGS_2026-11-10.md — an unstable hash
// reads as progress, so failStaleTasks resets lastProgress and never auto-fails.
func TestTaskProgressHash_StableForSameState(t *testing.T) {
	graph := &schemas.TaskGraph{
		TaskID: "task_1",
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, RetryCount: 0},
			"s2": {ID: "s2", Status: schemas.StepPending, RetryCount: 0},
		},
	}

	hash1 := taskProgressHash(graph)
	for i := 0; i < 64; i++ {
		if got := taskProgressHash(graph); got != hash1 {
			t.Fatalf("round %d: same state produced a different hash: %s vs %s", i, got, hash1)
		}
	}

	// Change a step status — hash should differ.
	graph.Steps["s1"].Status = schemas.StepOK
	hash3 := taskProgressHash(graph)
	if hash1 == hash3 {
		t.Errorf("different state produced same hash: %s", hash1)
	}

	// Change retry count — hash should differ.
	graph.Steps["s1"].Status = schemas.StepRunning
	graph.Steps["s1"].RetryCount = 1
	hash4 := taskProgressHash(graph)
	if hash1 == hash4 {
		t.Errorf("different retry count produced same hash: %s", hash1)
	}

	// Change graph status — hash should differ.
	graph.Steps["s1"].RetryCount = 0
	graph.Status = schemas.GraphFailed
	hash5 := taskProgressHash(graph)
	if hash1 == hash5 {
		t.Errorf("different graph status produced same hash: %s", hash1)
	}
}

// TestStaleTaskAutoFailTimeout_Default verifies that the default timeout
// is 24h when no config is set.
func TestStaleTaskAutoFailTimeout_Default(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	got := s.staleTaskAutoFailTimeout()
	if got != 24*time.Hour {
		t.Errorf("default timeout = %v, want 24h", got)
	}
}

// TestStaleTaskAutoFailTimeout_Configurable verifies that the timeout
// can be configured via System.StaleTaskAutoFailHours, and that <=0
// disables the monitor.
func TestStaleTaskAutoFailTimeout_Configurable(t *testing.T) {
	s, _ := newAnswerTestEngine(t)

	// Explicit value.
	hours := 48
	s.registry.System.StaleTaskAutoFailHours = &hours
	if got := s.staleTaskAutoFailTimeout(); got != 48*time.Hour {
		t.Errorf("timeout with 48h config = %v, want 48h", got)
	}

	// Zero disables.
	zero := 0
	s.registry.System.StaleTaskAutoFailHours = &zero
	if got := s.staleTaskAutoFailTimeout(); got != 0 {
		t.Errorf("timeout with 0 config = %v, want 0 (disabled)", got)
	}

	// Negative disables.
	neg := -1
	s.registry.System.StaleTaskAutoFailHours = &neg
	if got := s.staleTaskAutoFailTimeout(); got != 0 {
		t.Errorf("timeout with -1 config = %v, want 0 (disabled)", got)
	}
}

// TestFailStaleTasks_AutoFailsStaleTask verifies that a running task
// with no progress for the timeout period is auto-failed with
// GraphFailed status.
func TestFailStaleTasks_AutoFailsStaleTask(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	s.staleTaskSnapshots = make(map[string]*staleTaskState)

	taskID := "stale_task_1"
	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, RetryCount: 0},
		},
	}
	injectTestGraph(t, s, graph)

	// First call: records the snapshot (progress seen).
	s.failStaleTasks(time.Hour)

	snap, exists := s.staleTaskSnapshots[taskID]
	if !exists {
		t.Fatal("first check should record a snapshot")
	}

	// Simulate no progress: set lastProgress far enough back.
	snap.lastProgress = time.Now().Add(-2 * time.Hour)

	// Second call: same hash, past timeout → auto-fail.
	s.failStaleTasks(time.Hour)

	if graph.Status != schemas.GraphFailed {
		t.Errorf("graph status = %s, want GraphFailed", graph.Status)
	}
	if _, exists := s.staleTaskSnapshots[taskID]; exists {
		t.Error("snapshot should be deleted after auto-fail")
	}
}

// TestFailStaleTasks_NoFalsePositiveOnProgress verifies that a task
// that HAS made progress (hash changed) is NOT auto-failed, even if
// the previous snapshot is old.
func TestFailStaleTasks_NoFalsePositiveOnProgress(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	s.staleTaskSnapshots = make(map[string]*staleTaskState)

	taskID := "progress_task_1"
	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, RetryCount: 0},
		},
	}
	injectTestGraph(t, s, graph)

	// First call: records the snapshot.
	s.failStaleTasks(time.Hour)

	// Simulate old lastProgress but then change the graph state.
	s.staleTaskSnapshots[taskID].lastProgress = time.Now().Add(-2 * time.Hour)
	graph.Steps["s1"].Status = schemas.StepOK // progress!

	// Second call: hash changed → update snapshot, don't fail.
	s.failStaleTasks(time.Hour)

	if graph.Status != schemas.GraphRunning {
		t.Errorf("graph status = %s, want GraphRunning (progress was made)", graph.Status)
	}
	snap := s.staleTaskSnapshots[taskID]
	if snap == nil {
		t.Fatal("snapshot should be updated, not deleted")
	}
	// lastProgress should be reset to now since progress was detected.
	if time.Since(snap.lastProgress) > time.Minute {
		t.Errorf("lastProgress should be recent after progress detected, got %v ago", time.Since(snap.lastProgress))
	}
}

// TestFailStaleTasks_SkipsNonRunningTasks verifies that only GraphRunning
// tasks are checked; completed, failed, or cancelled tasks are ignored.
func TestFailStaleTasks_SkipsNonRunningTasks(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	s.staleTaskSnapshots = make(map[string]*staleTaskState)

	for _, status := range []schemas.GraphStatus{
		schemas.GraphCompleted,
		schemas.GraphFailed,
		schemas.GraphCancelled,
		schemas.GraphSuspended,
	} {
		taskID := "task_" + string(status)
		graph := &schemas.TaskGraph{
			TaskID: taskID,
			Status: status,
			Steps: map[string]*schemas.Step{
				"s1": {ID: "s1", Status: schemas.StepOK, RetryCount: 0},
			},
		}
		injectTestGraph(t, s, graph)
	}

	// Should not create any snapshots for non-running tasks.
	s.failStaleTasks(time.Hour)

	for _, status := range []schemas.GraphStatus{
		schemas.GraphCompleted,
		schemas.GraphFailed,
		schemas.GraphCancelled,
		schemas.GraphSuspended,
	} {
		taskID := "task_" + string(status)
		if _, exists := s.staleTaskSnapshots[taskID]; exists {
			t.Errorf("non-running task %s should not have a snapshot", taskID)
		}
		g := s.GetTaskGraph(taskID)
		if g.Status != status {
			t.Errorf("task %s status changed from %s to %s", taskID, status, g.Status)
		}
	}
}

// TestStartStaleTaskMonitor_DisabledWhenZero verifies that the monitor
// does not start when the timeout is configured as 0 (disabled).
func TestStartStaleTaskMonitor_DisabledWhenZero(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	s.staleTaskSnapshots = make(map[string]*staleTaskState)

	zero := 0
	s.registry.System.StaleTaskAutoFailHours = &zero

	// startStaleTaskMonitor should return immediately without starting
	// a goroutine when timeout is 0.
	s.startStaleTaskMonitor()

	// No goroutine should be running. We verify by checking that
	// bgWg doesn't block — if a goroutine was started, bgWg would
	// have an outstanding count.
	//
	// This is a weak test, but the stronger guarantee is that
	// failStaleTasks is never called (which we can't directly observe
	// without instrumentation). The key behavior is that timeout <= 0
	// returns early.
	done := make(chan struct{})
	go func() {
		s.bgWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Good: no goroutines were started.
	case <-time.After(time.Second):
		t.Fatal("bgWg.Wait() timed out — a goroutine was started despite disabled config")
	}
}

// TestStaleTaskMonitor_Integration verifies the full lifecycle: a stale
// task is auto-failed, and a progressing task survives.
func TestStaleTaskMonitor_Integration(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	s.staleTaskSnapshots = make(map[string]*staleTaskState)

	// Stale task: running, no progress.
	staleID := "stale_integration"
	staleGraph := &schemas.TaskGraph{
		TaskID: staleID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, RetryCount: 0},
		},
	}
	injectTestGraph(t, s, staleGraph)

	// Progressing task: running, will change state.
	progressID := "progress_integration"
	progressGraph := &schemas.TaskGraph{
		TaskID: progressID,
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning, RetryCount: 0},
		},
	}
	injectTestGraph(t, s, progressGraph)

	// First check: both get snapshots.
	s.failStaleTasks(time.Hour)

	if _, ok := s.staleTaskSnapshots[staleID]; !ok {
		t.Fatal("stale task should have a snapshot after first check")
	}
	if _, ok := s.staleTaskSnapshots[progressID]; !ok {
		t.Fatal("progress task should have a snapshot after first check")
	}

	// Simulate staleness for the stale task.
	s.staleTaskSnapshots[staleID].lastProgress = time.Now().Add(-2 * time.Hour)

	// Make progress on the progressing task.
	progressGraph.Steps["s1"].Status = schemas.StepOK

	// Second check: stale task fails, progress task survives.
	s.failStaleTasks(time.Hour)

	if staleGraph.Status != schemas.GraphFailed {
		t.Errorf("stale task status = %s, want GraphFailed", staleGraph.Status)
	}
	if progressGraph.Status != schemas.GraphRunning {
		t.Errorf("progress task status = %s, want GraphRunning", progressGraph.Status)
	}
}

// newLedgerSweepEngine wires a real SQLite ledger onto the monitor test engine
// and hands back the raw handle so a test can age a row. The default
// recordingTaskRegistry embeds a nil store.TaskRegistry, so it does not satisfy
// store.StaleTaskSweeper — that is the skip path file mode and mocks take, and
// the seven tests above exercise it without knowing they do.
func newLedgerSweepEngine(t *testing.T) (*DirectedEngine, *sql.DB) {
	t.Helper()
	s := newStaleMonitorTestEngine(t)
	db, err := store.InitDB(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s.SetTaskRegistry(store.NewSQLiteTaskBackend(db))
	return s, db
}

// seedAgedRunningRow writes a ledger row whose last write is four days old, the
// shape measured on task_eb209a68 ('running' since 2026-11-06 12:12).
func seedAgedRunningRow(t *testing.T, db *sql.DB, taskID string) {
	t.Helper()
	aged := time.Now().UTC().Add(-96 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO tasks (task_id, status, updated_at) VALUES (?, 'running', ?)`,
		taskID, aged); err != nil {
		t.Fatalf("seed %s: %v", taskID, err)
	}
}

func readLedgerStatus(t *testing.T, db *sql.DB, taskID string) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(context.Background(),
		`SELECT status FROM tasks WHERE task_id = ?`, taskID).Scan(&status); err != nil {
		t.Fatalf("read %s: %v", taskID, err)
	}
	return status
}

// TestStaleTaskMonitor_SweepsOrphanedLedgerRow is the F10 regression. A row left
// 'running' by a process that died is not in s.graphs and nothing rehydrates it,
// so the in-process loop never sees it and the frontend polls a task that will
// never finish.
// Bug: docs/plan/TELEGRAM_CALLBACK_AND_VCP_FINDINGS_2026-11-10.md F10.
func TestStaleTaskMonitor_SweepsOrphanedLedgerRow(t *testing.T) {
	s, db := newLedgerSweepEngine(t)
	const taskID = "task_orphan"
	seedAgedRunningRow(t, db, taskID)
	if g := s.GetTaskGraph(taskID); g != nil {
		t.Fatal("the row must not be in s.graphs — that absence is the whole of F10")
	}

	s.failStaleTasks(time.Hour)

	if got := readLedgerStatus(t, db, taskID); got != "failed" {
		t.Fatalf("orphaned row status = %s, want failed: the monitor only walked s.graphs", got)
	}
}

// TestStaleTaskMonitor_SweepReconcilesInMemoryGraph covers the divergence the
// sweep would otherwise introduce: GetStatus answers from s.graphs first
// (core/scheduler_status.go:73), so closing only the row leaves the UI showing
// 'running' until the in-process loop ages the same task out on its own
// first-sight clock. The executor must stop too, not just be muted.
func TestStaleTaskMonitor_SweepReconcilesInMemoryGraph(t *testing.T) {
	s, db := newLedgerSweepEngine(t)
	const taskID = "task_diverged"
	seedAgedRunningRow(t, db, taskID)

	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": {ID: "s1", Status: schemas.StepRunning}},
	}
	injectTestGraph(t, s, graph)
	cancelled := false
	s.cancelFuncs = map[string]context.CancelFunc{taskID: func() { cancelled = true }}

	// First sight: the in-process loop only records a snapshot here, so anything
	// that fails this task did it via the ledger sweep.
	s.failStaleTasks(time.Hour)

	if graph.Status != schemas.GraphFailed {
		t.Errorf("in-memory status = %s, want GraphFailed", graph.Status)
	}
	if !cancelled {
		t.Error("executor context was not cancelled: a muted-but-running task still burns tokens")
	}
	if got := readLedgerStatus(t, db, taskID); got != "failed" {
		t.Errorf("ledger status = %s, want failed", got)
	}
	if _, exists := s.staleTaskSnapshots[taskID]; exists {
		t.Error("snapshot for a closed task should be reclaimed")
	}
}

// Ensure the test engine has staleTaskSnapshots initialized.
// newAnswerTestEngine doesn't set this field, so tests that need it
// must initialize it themselves (as done above). This is a helper to
// avoid repetition in future tests.
func newStaleMonitorTestEngine(t *testing.T) *DirectedEngine {
	t.Helper()
	s, _ := newAnswerTestEngine(t)
	s.staleTaskSnapshots = make(map[string]*staleTaskState)
	return s
}

// Suppress unused import warning for config (used indirectly via
// newAnswerTestEngine which sets registry.System).
var _ = config.SystemSettings{}
var _ = sync.WaitGroup{}
var _ = context.Background
