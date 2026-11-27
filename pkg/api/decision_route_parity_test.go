package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/tools"
)

// Parity pins for register O41, plus the helpers the O42 pins reuse
// (batch B6-1, 2026-11-05).
//
// Q4-5 put type-first routing on orchestrator_submit_decision (1507f5b60), but
// the other two surfaces that answer a parked decision kept the pre-Q4-5
// heuristic or, worse, never had the branch at all:
//   - O41: POST /api/tasks/:id/decision routed on `choice=="fulfill" || output!=""`,
//     so a browser sending a payload-bearing retry landed in the delegation-only
//     FulfillStep — the same misrouting O17 documents, on the surface that answers
//     human_approval_required decisions.
//   - O42: Telegram /decision advertised `fulfill` in its usage line and then sent
//     every choice to SubmitDecisionWithPayload, whose engine switch has no
//     fulfill case. The advertised option could never be honored.
//
// These pin the routing decision itself, not a full answer round-trip: the two
// discriminations below are the ones that differ between the old and new code
// without needing a store, a spawner or a live DAG.

// newRoutingEngine is newTestEngine plus the shutdown the shared helper omits.
// Answering a decision persists a manifest under outputBase, and on Windows the
// logger's open handle makes t.TempDir's automatic RemoveAll fail with "directory
// is not empty" -- a teardown artifact, not a test failure. Same reason
// handlers_extra_test.go closes the logger and stops the engine.
func newRoutingEngine(t *testing.T) *core.DirectedEngine {
	t.Helper()
	reg, err := config.NewRegistry("")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	dir, err := os.MkdirTemp("", "decision_route_parity-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	logger, err := obs.NewLogger(dir, nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	engine := core.NewDirectedEngine(reg, nil, nil, nil, logger, nil, dir, dir, nil)
	t.Cleanup(func() {
		engine.Stop()
		logger.Close()
		_ = os.RemoveAll(dir)
	})
	return engine
}

func decisionRouteGraph() *schemas.TaskGraph {
	return &schemas.TaskGraph{
		TaskID: "t-route-parity",
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{"s1": {ID: "s1", Status: schemas.StepBlocked}},
		PendingDecisions: []*schemas.Decision{
			{ID: "dec_failed", StepID: "s1", Type: schemas.DecisionStepFailed},
			{ID: "dec_deleg", StepID: "s1", Type: schemas.DecisionDelegationRequired},
		},
	}
}

// O41: the answer that used to be hijacked by its own payload.
func TestTaskDecisionHandler_O41PayloadBearingRetryIsNotFulfill(t *testing.T) {
	engine := newRoutingEngine(t)
	engine.RegisterGraphForTest(decisionRouteGraph())
	h := &APIHandler{App: &tools.App{Scheduler: engine}}

	rr := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{
		"decision_id": "dec_failed",
		"choice":      "retry",
		"output":      "{}",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/t-route-parity/decision", strings.NewReader(string(body)))
	h.TaskDecisionHandler(rr, req)

	if rr.Code == http.StatusInternalServerError && strings.Contains(rr.Body.String(), "not a delegation") {
		t.Fatalf("regression to O41: the payload routed this onto the delegation-only fulfill path: %s", rr.Body.String())
	}
	if rr.Code == http.StatusOK {
		var resp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad JSON from handler: %v (%s)", err, rr.Body.String())
		}
		if resp["output_ignored"] != true {
			t.Fatalf("a dropped payload must be reported, not swallowed: %v", resp)
		}
	}
}

// O41: an id the engine cannot resolve keeps the payload fallback (Q4-5's
// backward-compatibility half), so the fulfill path is still reachable.
func TestTaskDecisionHandler_O41UnknownIDKeepsPayloadFallback(t *testing.T) {
	engine := newRoutingEngine(t)
	engine.RegisterGraphForTest(decisionRouteGraph())
	h := &APIHandler{App: &tools.App{Scheduler: engine}}

	rr := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{
		"decision_id": "dec_never_parked",
		"choice":      "retry",
		"output":      "{}",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/t-route-parity/decision", strings.NewReader(string(body)))
	h.TaskDecisionHandler(rr, req)

	// FulfillStep answers with "decision ... not found" — proof the call went to
	// the fulfill path by the fallback rule rather than to the decision path.
	if rr.Code == http.StatusOK || !strings.Contains(rr.Body.String(), "not found") {
		t.Fatalf("expected the fallback to reach FulfillStep, got %d: %s", rr.Code, rr.Body.String())
	}
}

// The Telegram-side pins for the same router reuse newRoutingEngine and
// decisionRouteGraph from this file.
