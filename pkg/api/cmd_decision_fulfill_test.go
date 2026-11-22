package api

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/tools"
)

// Regression pins for register O42 (batch B6-2, 2026-11-05). Both use the engine
// helper and the parked-decision graph from decision_route_parity_test.go.

// O42: fulfill with no payload must ask for the payload, not fail as an unknown
// choice on the decision path.
func TestCmdDecision_O42FulfillNeedsPayload(t *testing.T) {
	engine := newRoutingEngine(t)
	engine.RegisterGraphForTest(decisionRouteGraph())
	h := &ChatHandler{App: &tools.App{Scheduler: engine}}

	_, resp := h.cmdDecision("t-route-parity dec_deleg fulfill")
	if !strings.Contains(resp, "needs the result JSON") {
		t.Fatalf("fulfill without a payload should report the missing argument, got: %s", resp)
	}
	if strings.Contains(resp, "unknown choice") {
		t.Fatalf("regression to O42: fulfill fell through to the decision path: %s", resp)
	}
}

// O42: fulfill is handled by the delegation path. The probe is a fulfill with a
// payload aimed at a step_failed decision, which is the one call shape whose
// error message differs between the two entry points without needing a store, a
// spawner or a live DAG: FulfillStep answers "…not a delegation" (misrouted-
// fulfill, O17's wording) while SubmitDecisionWithPayload answers
// "unknown choice". A fulfilled delegation itself writes taskStore.Set and can
// only be proven with a real store, so it stays unpinned here.
func TestCmdDecision_O42FulfillReachesDelegationPath(t *testing.T) {
	engine := newRoutingEngine(t)
	engine.RegisterGraphForTest(decisionRouteGraph())
	h := &ChatHandler{App: &tools.App{Scheduler: engine}}

	_, resp := h.cmdDecision(`t-route-parity dec_failed fulfill {"result":"x"}`)
	if !strings.Contains(resp, "not a delegation") {
		t.Fatalf("expected FulfillStep's own misrouting error, got: %s", resp)
	}
	if strings.Contains(resp, "unknown choice") {
		t.Fatalf("regression to O42: fulfill is still handled by the decision path: %s", resp)
	}
}
