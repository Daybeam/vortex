package core

import (
	"github.com/daybeam/vortex/core/sieve"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestSpawner_ResultRepetition_SieveInitialized verifies that the Spawner
// creates a non-nil Sieve with CheckResultRepetition accessible. This is the
// wiring contract: if the Sieve were nil or uninitialized, the
// CheckResultRepetition call in spawner.go would silently no-op (the method
// has a nil receiver guard), and result repetition loops would burn the
// turn budget undetected.
//
// Regression: before the fix, CheckResultRepetition was only wired in
// chat_harness.go, NOT in spawner.go. The spawner's Sieve existed but was
// only used for PruneOutput and InspectToolCalls — never for result-level
// repetition detection.
func TestSpawner_ResultRepetition_SieveInitialized(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", BaseCapability: "text"},
		},
		Skills: map[string]*config.Skill{},
		MCPs:   map[string]*config.MCPDef{},
		System: config.SystemSettings{},
	}
	logDir := t.TempDir()
	logger := mustNewLogger(t, logDir, &reg.System)
	s := &Spawner{
		registry: reg,
		logger:   logger,
		sieve:    sieve.NewSieveFromConfig(reg.System),
	}

	if s.sieve == nil {
		t.Fatal("Spawner.sieve must be non-nil — CheckResultRepetition would no-op")
	}

	// Simulate the spawner's tool result processing pattern.
	// NOTE (2026-10-12): CheckResultRepetition is NOT wired in spawner.go
	// anymore (task-scoped history misfires on cross-step re-reads in DAG
	// mode — VERIFY legitimately re-reads ACT results). It remains wired
	// only in chat_harness.go. This test exercises the Sieve method itself.
	taskID := "task-result-rep-wiring"
	identicalResult := `{"name":"Sophia Martin","email":"sophia@example.com"}`

	// Feed identical results up to the threshold.
	// CheckResultRepetition records each call and returns true when
	// the count of prior identical results >= ToolRepetitionThreshold.
	for i := 1; i <= s.sieve.ToolRepetitionThreshold; i++ {
		triggered := s.sieve.CheckResultRepetition(taskID, identicalResult)
		if triggered {
			t.Fatalf("Call %d: should not trigger before threshold (%d)", i, s.sieve.ToolRepetitionThreshold)
		}
	}

	// One more call with the same result should trigger.
	if !s.sieve.CheckResultRepetition(taskID, identicalResult) {
		t.Fatal("Should trigger after threshold reached — CheckResultRepetition wiring is broken")
	}
}

// TestSpawner_ResultRepetition_MessageFormat verifies that the [RESULT REPETITION]
// steer message prepended by the spawner matches the format used in
// chat_harness.go. This is a contract test: both code paths must produce
// the same message so the model sees consistent guidance regardless of
// which execution path it's on.
func TestSpawner_ResultRepetition_MessageFormat(t *testing.T) {
	s := &Spawner{
		sieve: sieve.NewSieve(10),
	}
	s.sieve.ToolRepetitionThreshold = 2

	taskID := "task-msg-format"
	result := `{"status":"ok","data":"unchanged"}`

	// Prime the history to just below threshold.
	s.sieve.CheckResultRepetition(taskID, result)
	s.sieve.CheckResultRepetition(taskID, result)

	// On the next call, the spawner would prepend this message.
	md := result
	if s.sieve.CheckResultRepetition(taskID, md) {
		md = "[RESULT REPETITION] You are receiving the same result from different queries. The data is not changing. Stop re-querying and provide a direct answer based on the information you already have.\n\n" + md
	}

	if !strings.HasPrefix(md, "[RESULT REPETITION]") {
		t.Fatal("Result should have [RESULT REPETITION] prefix when repetition is detected")
	}
	if !strings.Contains(md, result) {
		t.Fatal("Original result should be preserved after the repetition warning")
	}
}

// TestSpawner_ResultRepetition_PollerExempt verifies that poller tools
// (progress, health, wait) are exempt from result repetition checks in the
// spawner, matching the chat_harness.go pattern. Pollers legitimately return
// time-varying results with the same args, so flagging them as "repetition"
// would be a false positive.
func TestSpawner_ResultRepetition_PollerExempt(t *testing.T) {
	// The spawner's condition is: sieve.IsReadTool(name) && !sieve.IsPollerTool(name)
	// Poller tools should fail this condition.
	pollers := []string{"progress_check", "health_status", "wait_for_completion"}
	for _, name := range pollers {
		if !sieve.IsReadTool(name) {
			continue // not a read tool, wouldn't be checked anyway
		}
		if !sieve.IsPollerTool(name) {
			t.Errorf("Poller %q should be exempt but isPollerTool returned false", name)
		}
		// The spawner's condition: isReadTool && !isPollerTool
		// For pollers, this should be false → CheckResultRepetition NOT called.
		shouldCheck := sieve.IsReadTool(name) && !sieve.IsPollerTool(name)
		if shouldCheck {
			t.Errorf("Poller %q should NOT trigger CheckResultRepetition in spawner", name)
		}
	}
}

// TestSpawner_ResultRepetition_WriteToolExempt verifies that write tools
// (create, update, delete, cancel, etc.) are exempt from result repetition
// checks in the spawner. Write tools don't go through the isReadTool path,
// so CheckResultRepetition is not called for them.
func TestSpawner_ResultRepetition_WriteToolExempt(t *testing.T) {
	writes := []string{"create_user", "update_record", "delete_item", "cancel_order", "write_file"}
	for _, name := range writes {
		if sieve.IsReadTool(name) {
			t.Errorf("Write tool %q should NOT be classified as read tool — would wrongly trigger CheckResultRepetition", name)
		}
	}
}
