package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// TestReflectOnTask_CancelledLifecycleCtxReturnsPromptly is the regression
// test for audit C2: ReflectOnTask must accept a lifecycleCtx and thread it
// into inner goroutines (hybrid merge, crystallization) so they exit on
// shutdown instead of running for up to 5 minutes on a dead engine.
//
// This test verifies the signature change compiles and that a pre-cancelled
// lifecycle context doesn't cause ReflectOnTask to hang. If someone reverts
// the context parameter, this test won't compile.
func TestReflectOnTask_CancelledLifecycleCtxReturnsPromptly(t *testing.T) {
	// Use os.MkdirTemp for all dirs to avoid t.TempDir() auto-cleanup race
	// with reflection engine goroutines on Windows.
	tmpDir, _ := os.MkdirTemp("", "reflect-audit-")
	reg, _ := config.NewRegistry(filepath.Join(tmpDir, "config.json"))
	ts := &mockSignalTaskStore{}
	es, _ := store.NewExperienceStore(tmpDir, nil, nil, nil, nil)
	loggerDir, _ := os.MkdirTemp("", "reflect-audit-log-")
	logger, _ := NewLogger(loggerDir, nil)
	defer logger.Close()

	re := NewReflectionEngine(es, ts, reg, nil, logger)

	// Pre-cancel the lifecycle context — simulates engine shutdown.
	lifecycleCtx, cancel := context.WithCancel(context.Background())
	cancel()

	graph := &schemas.TaskGraph{
		TaskID: "task_shutdown_test",
		Status: schemas.GraphCompleted,
		Steps: map[string]*schemas.Step{
			"s1": {
				ID:     "s1",
				RoleID: "r1",
				Task:   "test task",
				Status: schemas.StepOK,
			},
		},
	}

	// ReflectOnTask should return within a few seconds even though the
	// inner goroutines have 5-minute timeouts — they derive from the
	// cancelled lifecycleCtx and exit immediately.
	done := make(chan struct{})
	go func() {
		re.ReflectOnTask(lifecycleCtx, graph)
		close(done)
	}()

	select {
	case <-done:
		// Success — returned promptly.
	case <-time.After(10 * time.Second):
		t.Fatal("ReflectOnTask hung for >10s with a cancelled lifecycle context — " +
			"inner goroutines likely use context.Background() instead of lifecycleCtx (audit C2)")
	}
}
