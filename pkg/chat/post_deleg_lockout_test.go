package chat

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestPostDelegModeFromEnv(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", PostDelegOff},
		{"off", PostDelegOff},
		{"0", PostDelegOff},
		{"soft", PostDelegSoft},
		{"SOFT", PostDelegSoft},
		{"1", PostDelegSoft},
		{"true", PostDelegSoft},
		{"hard", PostDelegHard},
		{"HARD", PostDelegHard},
		// A typo must leave behaviour alone, not choose an arm.
		{"hrad", PostDelegOff},
		{"yes", PostDelegOff},
		{" hard ", PostDelegHard},
	}
	for _, c := range cases {
		t.Setenv("VORTEX_POST_DELEG_LOCKOUT", c.raw)
		if got := postDelegModeFromEnv(); got != c.want {
			t.Errorf("VORTEX_POST_DELEG_LOCKOUT=%q -> %q, want %q", c.raw, got, c.want)
		}
	}
}

// The default arm must not change anything: same tool table, no interception.
func TestPostDelegOffIsInert(t *testing.T) {
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "")
	h := &ChatHarness{}
	h.pd.recordDelegation("task_x") // even with a task id recorded, off must not lock

	if h.pd.blockedBy("execute_code") {
		t.Fatal("off arm must never intercept, even when a task id is recorded")
	}
	if h.pd.blocked != 0 {
		t.Fatalf("off arm must not count, got %d", h.pd.blocked)
	}
	if !hasTool(h.ToolDefinitions(), "execute_code") {
		t.Fatal("off arm must keep execute_code in the table")
	}
}

func TestBlockedByPostDeleg(t *testing.T) {
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "soft")
	h := &ChatHarness{}
	h.pd.recordDelegation("task_abc")

	for _, tool := range []string{"execute_code", "write_file", "batch_replace"} {
		if !h.pd.blockedBy(tool) {
			t.Errorf("%s must be intercepted after delegation", tool)
		}
	}
	// The escape hatches must survive, or the lockout strands the run.
	// read_file is one of them: it cannot produce a deliverable, and without it
	// the parent cannot tell whether the sub-task's work is any good (722
	// post-delegation read_file calls, 0 of them producing).
	for _, tool := range []string{
		"read_file",
		"orchestrator_task_status",
		"orchestrator_help",
		"search_archive",
		"delegate_to_orchestrator",
	} {
		if h.pd.blockedBy(tool) {
			t.Errorf("%s must NOT be intercepted: it is how the result is collected", tool)
		}
	}
	if h.pd.blocked != 3 {
		t.Fatalf("counted %d interceptions, want 3", h.pd.blocked)
	}
}

func TestBlockedByPostDelegRequiresDelegation(t *testing.T) {
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "soft")
	h := &ChatHarness{} // no task id yet
	if h.pd.blockedBy("execute_code") {
		t.Fatal("must not intercept before anything was delegated")
	}
}

// calls_after_deleg and post_deleg_blocked must count disjoint sets: "work that
// stayed in the foreground" and "work the model tried to keep" are different
// quantities (iron law 229). A blocked call ran nothing, so it must not also
// inflate the executed count — otherwise soft would look identical to off and
// the lever's effect would be invisible in its own metric.
func TestExecutedAndBlockedAreDisjoint(t *testing.T) {
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "soft")
	h := &ChatHarness{}
	h.pd.recordDelegation("task_abc")

	for _, tool := range []string{"execute_code", "write_file", "batch_replace"} {
		if !h.pd.blockedBy(tool) {
			t.Fatalf("%s should have been blocked", tool)
		}
	}
	for _, tool := range []string{"orchestrator_task_status", "orchestrator_task_status", "read_file"} {
		if h.pd.blockedBy(tool) {
			t.Fatalf("%s should not have been blocked", tool)
		}
		h.pd.noteExecuted()
	}

	if h.pd.blocked != 3 {
		t.Errorf("blocked=%d, want 3 (only the work tools)", h.pd.blocked)
	}
	if h.pd.after != 3 {
		t.Errorf("after=%d, want 3 (only the calls that ran)", h.pd.after)
	}
}

// Nothing delegated ⇒ no post-delegation calls to attribute, on any arm.
func TestNoteExecutedNeedsDelegation(t *testing.T) {
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "soft")
	h := &ChatHarness{}
	h.pd.noteExecuted()
	if h.pd.after != 0 {
		t.Fatalf("after=%d before any delegation, want 0", h.pd.after)
	}
}

// The off arm must still measure calls_after_deleg: it is the baseline, and a
// baseline read off a different binary is not a baseline (iron law 223).
func TestOffArmStillCountsCallsAfterDelegation(t *testing.T) {
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "")
	h := &ChatHarness{}
	h.pd.recordDelegation("task_abc") // what execTool records on every arm
	h.pd.noteExecuted()
	h.pd.noteExecuted()
	if h.pd.after != 2 {
		t.Fatalf("off arm after=%d, want 2 — the baseline must come from the same binary", h.pd.after)
	}
	if h.pd.blocked != 0 {
		t.Fatalf("off arm blocked=%d, want 0", h.pd.blocked)
	}
}

// soft arm keeps the table intact — only the return value changes.
func TestSoftArmKeepsToolTable(t *testing.T) {
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "soft")
	h := &ChatHarness{}
	h.pd.recordDelegation("task_abc")
	for _, tool := range []string{"execute_code", "write_file", "batch_replace", "read_file"} {
		if !hasTool(h.ToolDefinitions(), tool) {
			t.Errorf("soft arm must keep %s visible", tool)
		}
	}
}

// hard arm removes the work tools and keeps the escape hatches.
func TestHardArmRemovesWorkTools(t *testing.T) {
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "hard")
	h := &ChatHarness{}
	h.pd.recordDelegation("task_abc")
	got := h.ToolDefinitions()

	for _, tool := range []string{"execute_code", "write_file", "batch_replace"} {
		if hasTool(got, tool) {
			t.Errorf("hard arm must remove %s", tool)
		}
	}
	for _, tool := range []string{"delegate_to_orchestrator", "orchestrator_task_status", "orchestrator_help", "read_file"} {
		if !hasTool(got, tool) {
			t.Errorf("hard arm must keep %s", tool)
		}
	}
}

// hard arm must not shrink the table before a delegation happened, otherwise
// the model can never make the call that triggers the shrink.
func TestHardArmBeforeDelegationKeepsTable(t *testing.T) {
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "hard")
	h := &ChatHarness{}
	if !hasTool(h.ToolDefinitions(), "execute_code") {
		t.Fatal("hard arm must not remove tools before a task was delegated")
	}
}

// The lockout message must name the task id and a next step; a bare "denied"
// makes the model retry the same tool.
func TestPostDelegLockoutMessageIsActionable(t *testing.T) {
	msg := postDelegLockoutMessage("execute_code", "task_abc")
	for _, want := range []string{"task_abc", "orchestrator_task_status", "execute_code"} {
		if !strings.Contains(msg, want) {
			t.Errorf("lockout message must mention %q, got:\n%s", want, msg)
		}
	}
}

func hasTool(tools []schemas.ToolDefinition, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
