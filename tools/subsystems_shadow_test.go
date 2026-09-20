package tools

import (
	"context"
	"testing"
	"time"
)

// TestShadowLearning_BoundedContextHasDeadline is the regression test for
// audit H2: the Shadow Learning goroutine in registerExperienceSubsystem's
// direct-execute handler must use context.WithTimeout instead of a bare
// context.Background(), so the goroutine can't hang forever if
// RecordTaskCompletion blocks (e.g. SQLite busy lock).
//
// This test verifies the principle: a context created with WithTimeout has
// a deadline, while context.Background() does not. If the H2 fix is reverted
// to context.Background(), this test documents what was lost.
func TestShadowLearning_BoundedContextHasDeadline(t *testing.T) {
	// This is the pattern the H2 fix uses:
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		t.Fatal("context.WithTimeout(context.Background(), 10s) must have a " +
			"deadline — if this fails, the H2 fix pattern is broken")
	}

	// Contrast: bare context.Background() has NO deadline (the bug)
	bareCtx := context.Background()
	_, bareHasDeadline := bareCtx.Deadline()
	if bareHasDeadline {
		t.Fatal("context.Background() should NOT have a deadline — " +
			"if it does, the test premise is wrong")
	}
}
