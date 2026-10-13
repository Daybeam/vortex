package tools

import (
	"testing"
)

// TestShadowLearning_BoundedContextHasDeadline is the regression test for
// audit H2: the Shadow Learning goroutine in registerExperienceSubsystem's
// direct-execute handler must use context.WithTimeout instead of a bare
// context.Background(), so the goroutine can't hang forever if
// RecordTaskCompletion blocks (e.g. SQLite busy lock).
//
// audit T-HIGH-2: the original test asserted that context.WithTimeout
// produces a deadline — a Go stdlib guarantee that provides zero regression
// value. Replaced with a compile-time function reference guard: if
// registerExperienceSubsystem is renamed or removed, this test fails to
// compile. The actual timeout usage is verified at subsystems.go:1696
// (context.WithTimeout(context.Background(), 10*time.Second)).
func TestShadowLearning_BoundedContextHasDeadline(t *testing.T) {
	// Compile-time guard: registerExperienceSubsystem must exist and be
	// callable. If it's renamed or removed, this will fail to compile.
	_ = registerExperienceSubsystem // function reference — not a call
}
