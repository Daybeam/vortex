package tools

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// Routing table for orchestrator_submit_decision (Q4-5, answered 2026-11-05).
//
// The bug these pin is O17: routing was inferred from the presence of an
// `output` payload, so any host that serialised a result object for a
// payload-free answer (get_task_status view=full hands decisions back as JSON,
// so `{}` is the natural thing to send) was answered by the delegation-only
// FulfillStep. The operator's ruling keeps that payload rule as the fallback
// and makes the decision's own type the primary key, so both halves are pinned
// here: the veto and the fallback.

type fakeTyper map[string]schemas.DecisionType

func (f fakeTyper) PendingDecisionType(_ string, decisionID string) (schemas.DecisionType, bool) {
	t, ok := f[decisionID]
	return t, ok
}

var q45Types = fakeTyper{
	"dec_step_failed": schemas.DecisionStepFailed,
	"dec_approval":    schemas.DecisionHumanApprovalRequired,
	"dec_delegation":  schemas.DecisionDelegationRequired,
}

// The fix itself: a payload-carrying answer to a non-delegation decision is no
// longer a delegation hand-back.
func TestRouteSubmitDecision_Q45TypeVetoesPayloadHeuristic(t *testing.T) {
	cases := []struct {
		name       string
		decisionID string
		choice     string
		output     string
	}{
		{"abort with empty object on step_failed", "dec_step_failed", "abort", "{}"},
		{"skip with reason on step_failed", "dec_step_failed", "skip", `{"reason":"done"}`},
		{"retry with note on approval", "dec_approval", "retry", `{"note":"go"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := routeSubmitDecision(q45Types, "t1", tc.decisionID, tc.choice, tc.output)
			if r.toFulfill {
				t.Fatalf("routed a %s answer to the delegation-only FulfillStep (O17)", tc.choice)
			}
			if !r.outputDropped {
				t.Fatal("outputDropped not recorded: the response would silently swallow a payload it discarded")
			}
			// The old rule, still exported and still the fallback, is what made
			// these calls land on the wrong path. Pinning the difference keeps
			// the intent of the fix visible if anyone re-tightens the router.
			if !shouldRouteToFulfill(tc.choice, tc.output) {
				t.Fatalf("expected shouldRouteToFulfill to still say yes for %q/%q", tc.choice, tc.output)
			}
		})
	}
}

// Backward compatibility: everything that reached the right path before Q4-5
// must still reach it. Delegation hand-backs, structured-payload choices, and
// payload-free answers are all unchanged.
func TestRouteSubmitDecision_Q45PreservesExistingPaths(t *testing.T) {
	cases := []struct {
		name        string
		typer       decisionTyper
		decisionID  string
		choice      string
		output      string
		wantFulfill bool
	}{
		// A real delegation hand-back still goes to FulfillStep with its payload.
		{"delegation fulfill", q45Types, "dec_delegation", "fulfill", `{"result":"ok"}`, true},
		{"delegation answered by payload alone", q45Types, "dec_delegation", "skip", `{"result":"ok"}`, true},
		// Explicit choice="fulfill" is intent, not a heuristic: it stays on the
		// fulfill path so the engine answers with misroutedFulfillError, which
		// names the decision's real type, instead of `unknown choice "fulfill"`.
		{"fulfill named on a step_failed decision", q45Types, "dec_step_failed", "fulfill", `{"result":"ok"}`, true},
		{"fulfill named with no payload", q45Types, "dec_step_failed", "fulfill", "", true},
		// T1 exclusions: structured payload must reach SubmitDecisionWithPayload.
		{"rewrite_dag with surgery payload", q45Types, "dec_step_failed", "rewrite_dag", `{"deprecated_step_id":"s1"}`, false},
		{"replan with payload", q45Types, "dec_step_failed", "replan", `{"replacement_steps":[]}`, false},
		{"create_role with payload", q45Types, "dec_step_failed", "create_role", `{"role_draft":{}}`, false},
		{"goal_change_requested with payload", q45Types, "dec_step_failed", "goal_change_requested", `{"intent":"x"}`, false},
		// Payload-free answers never routed to fulfill, and still do not.
		{"skip no payload", q45Types, "dec_step_failed", "skip", "", false},
		{"abort no payload", q45Types, "dec_step_failed", "abort", "", false},

		// Fallback half of the ruling: nothing to veto with.
		{"unknown decision id falls back to payload rule", q45Types, "dec_evicted", "abort", "{}", true},
		{"nil engine falls back to payload rule", nil, "dec_step_failed", "abort", "{}", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := routeSubmitDecision(tc.typer, "t1", tc.decisionID, tc.choice, tc.output)
			if r.toFulfill != tc.wantFulfill {
				t.Fatalf("toFulfill = %v, want %v", r.toFulfill, tc.wantFulfill)
			}
			if r.toFulfill && r.outputDropped {
				t.Fatal("a call that kept its payload must not report outputDropped")
			}
		})
	}
}

// The payload-free abort the caller should have sent all along still answers
// without a drop flag — Q4-5 must not make clean calls look like repaired ones.
func TestRouteSubmitDecision_Q45CleanAnswerUnmarked(t *testing.T) {
	r := routeSubmitDecision(q45Types, "t1", "dec_step_failed", "abort", "")
	if r.toFulfill || r.outputDropped {
		t.Fatalf("payload-free abort routed as %+v, want the plain decision path", r)
	}
}

// ResolveDecision is the same table handed to the non-MCP entry points (O41/O42).
// It takes the concrete engine, so this pins the two things the fake typer could
// not: that a real *DirectedEngine satisfies the read, and that a nil engine
// selects the fallback instead of panicking on a nil receiver.
func TestResolveDecision_RealEngineAndNilSafe(t *testing.T) {
	reg, err := config.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logger, err := obs.NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	engine := core.NewDirectedEngine(reg, nil, nil, nil, logger, nil, t.TempDir(), t.TempDir(), nil)
	// Windows: the logger keeps a handle under the temp dir, so t.TempDir's
	// automatic RemoveAll needs the engine and logger closed first.
	t.Cleanup(func() {
		engine.Stop()
		logger.Close()
	})
	engine.RegisterGraphForTest(&schemas.TaskGraph{
		TaskID: "t-route",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": {ID: "s1", Status: schemas.StepBlocked}},
		PendingDecisions: []*schemas.Decision{
			{ID: "dec_failed", StepID: "s1", Type: schemas.DecisionStepFailed},
			{ID: "dec_deleg", StepID: "s1", Type: schemas.DecisionDelegationRequired},
		},
	})

	cases := []struct {
		name           string
		engine         *core.DirectedEngine
		decisionID     string
		choice, output string
		wantFulfill    bool
		wantIgnored    bool
	}{
		// The O17 shape, now on the web/Telegram path: the type vetoes the payload.
		{"step_failed with payload stays off fulfill", engine, "dec_failed", "retry", "{}", false, true},
		{"delegation fulfilled by id", engine, "dec_deleg", "fulfill", `{"result":"x"}`, true, false},
		{"clean answer is unmarked", engine, "dec_failed", "abort", "", false, false},
		// Nil engine = no read available, so the payload rule must still carry a
		// delegation hand-back rather than failing the call outright.
		{"nil engine falls back to payload", nil, "dec_deleg", "fulfill", `{"result":"x"}`, true, false},
		{"nil engine with payload routes fulfill", nil, "dec_unknown", "retry", "{}", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotFulfill, gotIgnored := ResolveDecision(tc.engine, "t-route", tc.decisionID, tc.choice, tc.output)
			if gotFulfill != tc.wantFulfill || gotIgnored != tc.wantIgnored {
				t.Fatalf("ResolveDecision = (%v,%v), want (%v,%v)", gotFulfill, gotIgnored, tc.wantFulfill, tc.wantIgnored)
			}
		})
	}

	// An id that is not parked must resolve to the fallback, not to a guess.
	if f, _ := ResolveDecision(engine, "t-route", "dec_nope", "retry", "{}"); !f {
		t.Fatal("unresolvable decision id abandoned the payload fallback")
	}
}
