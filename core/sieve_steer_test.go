package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// newTestSpawner builds the minimum Spawner that steerRepeatedResult touches.
func newTestSpawner(t *testing.T) *Spawner {
	t.Helper()
	reg := &config.Registry{
		Roles: map[string]*config.Role{"worker": {ID: "worker", BaseCapability: "text"}},
		MCPs:  map[string]*config.MCPDef{},
	}
	return &Spawner{
		registry: reg,
		logger:   mustNewLogger(t, t.TempDir(), &reg.System),
		sieve:    newSieveFromConfig(reg.System),
	}
}

// TestSteerRepeatedResult_TwoStageEscalation pins what the model actually reads.
//
// The measured failure (4fab96f_3, 2026-10-19): 61 calls to show_transactions and
// 30 to show_sent_payment_requests, two distinct payloads, 91 of 102 calls pure
// re-reads, then MAX_TURNS_EXHAUSTED. Each duplicate also cost 7.4K characters of
// context, so by turn 40 the window was full of copies of two JSON blobs.
func TestSteerRepeatedResult_TwoStageEscalation(t *testing.T) {
	s := newTestSpawner(t)
	const tool = "venmo__show_transactions"
	const payload = `{"transactions":[{"id":1,"note":"Rent"}]}`

	// First sighting: untouched.
	if got := s.steerRepeatedResult("t1", "s1", tool, payload); got != payload {
		t.Fatalf("first result was modified:\n%s", got)
	}

	// Second: a nudge, payload still present so the model can act on it.
	second := s.steerRepeatedResult("t1", "s1", tool, payload)
	if !strings.HasPrefix(second, "[RESULT REPETITION]") {
		t.Errorf("second identical result got no steer:\n%s", second)
	}
	if !strings.Contains(second, payload) {
		t.Error("steer dropped the payload; the model would lose the only data it has")
	}
	if !strings.Contains(second, tool) {
		t.Error("steer does not name the tool, so it is not actionable")
	}

	// Third: still a steer (same stage).
	if got := s.steerRepeatedResult("t1", "s1", tool, payload); !strings.HasPrefix(got, "[RESULT REPETITION]") {
		t.Errorf("third identical result should still steer, got:\n%s", got)
	}

	// Fourth: withhold. The payload is already in context; a fourth copy is pure
	// displacement.
	fourth := s.steerRepeatedResult("t1", "s1", tool, payload)
	if strings.Contains(fourth, payload) {
		t.Errorf("fourth identical result still carried the %d-char payload", len(payload))
	}
	if !strings.Contains(fourth, "withheld") || !strings.Contains(fourth, tool) {
		t.Errorf("withhold message is not actionable:\n%s", fourth)
	}
	// The point of withholding is context relief, so the stub must be short in
	// absolute terms — not merely shorter than this toy payload. The payloads
	// that motivated the change were 7.4-7.6K characters each.
	if len(fourth) > 600 {
		t.Errorf("withhold stub is too long to relieve context pressure (%d chars)", len(fourth))
	}
}

// TestSteerRepeatedResult_Exemptions makes sure the guard does not reach tools
// where repetition is legitimate or meaningless.
func TestSteerRepeatedResult_Exemptions(t *testing.T) {
	s := newTestSpawner(t)
	const payload = "same"

	// Write tools: a write returning identical text is not a stuck read.
	for i := 0; i < 6; i++ {
		if got := s.steerRepeatedResult("t2", "s1", "venmo__remind_payment_request", payload); got != payload {
			t.Fatalf("write tool was steered on iteration %d", i)
		}
	}

	// Pollers: identical payloads are expected while waiting.
	for i := 0; i < 6; i++ {
		if got := s.steerRepeatedResult("t2", "s1", "job_wait_for_progress", payload); got != payload {
			t.Fatalf("poller was steered on iteration %d", i)
		}
	}

	// Nil sieve and empty payload must not panic or alter anything.
	empty := &Spawner{}
	if got := empty.steerRepeatedResult("t2", "s1", "venmo__show_transactions", payload); got != payload {
		t.Error("nil sieve changed the payload")
	}
	if got := s.steerRepeatedResult("t2", "s1", "venmo__show_transactions", ""); got != "" {
		t.Error("empty payload must pass through untouched")
	}
}
