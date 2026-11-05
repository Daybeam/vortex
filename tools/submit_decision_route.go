package tools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// submit_decision routing (Q4-5, answered 2026-11-05).
//
// The operator's ruling: 「按 decision_id 路由，同时保留按载荷存在与否路由作兜底。
// 这条路径报错信息误导过人，值得修，但要保持 MCP 面向后兼容。」
//
// So: the parked decision's own type is the primary key for choosing the engine
// entry point, and the old payload-presence rule (shouldRouteToFulfill) survives
// only as the fallback for calls whose decision cannot be resolved. What that
// changes, and what it deliberately does not, is written next to each branch.

// decisionTyper is the engine read this router needs. Declared as an interface
// so the routing table is testable without building a DirectedEngine; the
// concrete provider is (*core.DirectedEngine).PendingDecisionType.
type decisionTyper interface {
	PendingDecisionType(taskID, decisionID string) (schemas.DecisionType, bool)
}

// submitDecisionRoute records which entry point answers one submit_decision
// call and whether a payload had to be dropped to answer it there.
type submitDecisionRoute struct {
	// toFulfill routes to FulfillStep: the delegation hand-back path, which is
	// the only path that consumes `output` as a result payload.
	toFulfill bool
	// outputDropped records that a non-empty `output` was carried to a
	// payload-free answer and discarded. The tool response surfaces it as
	// output_ignored, so the drop is visible instead of silent.
	outputDropped bool
}

// ResolveDecision is the exported view of the Q4-5 routing rule, for the entry
// points that are not the MCP tool (register O41 / O42): the HTTP console's
// POST /api/tasks/:id/decision and Telegram's /decision. Both used to keep the
// pre-Q4-5 heuristic — route on the mere presence of an `output` payload — so
// the same choice on the same parked decision resolved one way over MCP and
// another way over web or Telegram. Handing them this function is the whole fix;
// the branches and their reasons are documented on routeSubmitDecision.
//
// A nil engine is passed straight through as "cannot resolve", which selects the
// payload fallback rather than guessing.
func ResolveDecision(engine *core.DirectedEngine, taskID, decisionID, choice, output string) (toFulfill, outputIgnored bool) {
	var typer decisionTyper
	if engine != nil {
		typer = engine
	}
	route := routeSubmitDecision(typer, taskID, decisionID, choice, output)
	return route.toFulfill, route.outputDropped
}

func routeSubmitDecision(typer decisionTyper, taskID, decisionID, choice, output string) submitDecisionRoute {
	if !shouldRouteToFulfill(choice, output) {
		return submitDecisionRoute{}
	}

	// choice="fulfill" states the caller's intent outright, so it stays on the
	// delegation path even when the decision turns out not to be a delegation:
	// FulfillStep answers it with misroutedFulfillError, which names the real
	// decision type and the payload rule. Rerouting here would trade that for
	// the blander `unknown choice "fulfill"` from the decision path.
	if choice == "fulfill" {
		return submitDecisionRoute{toFulfill: true}
	}

	// Reaching here means the ONLY reason this call looked like a delegation was
	// a non-empty `output`. Ask the engine what the decision actually is.
	//
	// O17 (docs/audit/unwired-code-register.md §O17): get_task_status view=full
	// hands decisions back as JSON objects, so a host that serialises the result
	// it was told to produce — `{}` for an abort — landed in the delegation-only
	// FulfillStep and got an error about delegations. That error sent me into a
	// false finding ("a step_failed decision can be parked but never answered").
	if typer != nil {
		if decType, known := typer.PendingDecisionType(taskID, decisionID); known && decType != schemas.DecisionDelegationRequired {
			return submitDecisionRoute{outputDropped: true}
		}
	}

	// Fallback, kept on purpose: unknown task, an id that is no longer parked,
	// or a nil engine. Routing these by payload preserves every call shape that
	// worked before Q4-5; the engine's own type gate stays the authority that
	// rejects a non-delegation answer on the fulfill path.
	return submitDecisionRoute{toFulfill: true}
}

// handleSubmitDecision is the orchestrator_submit_decision handler, extracted
// from registerTaskList so the routing fix does not add lines to a function the
// size gate already has frozen (Q6-1: new behaviour goes into helpers).
func handleSubmitDecision(app *App, ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	tid := strArg(req.Params.Arguments, "task_id")
	if DenyIfNotOwner(ctx, app, tid) {
		return errResult("not found")
	}
	did := strArg(req.Params.Arguments, "decision_id")
	choice := strArg(req.Params.Arguments, "choice")
	output := strArg(req.Params.Arguments, "output")

	args := map[string]any{}
	if req.Params.Arguments != nil {
		for k, v := range req.Params.Arguments.(map[string]any) {
			args[k] = v
		}
	}
	humanApproval, _ := args["human_approval"].(map[string]any)
	hasApproval := humanApproval != nil && len(humanApproval) > 0

	// Public tier choice allowlist.
	// T2: replan/create_role are open to public tier BUT public callers
	// cannot inject payload — replan uses pre-computed planner plan,
	// create_role uses GenerateRoleObjects.
	if err := validateTierChoice(EffectiveTier(ctx, app), choice, output); err != nil {
		return errResult(err.Error())
	}

	// Route: by the decision's type where it resolves, by payload presence where
	// it does not (see routeSubmitDecision for the branches and why).
	var typer decisionTyper
	if app.Scheduler != nil {
		typer = app.Scheduler
	}
	route := routeSubmitDecision(typer, tid, did, choice, output)

	if route.toFulfill {
		if output == "" {
			return errResult("choice='fulfill' or output payload requires 'output' field with the result JSON")
		}
		if err := app.Scheduler.FulfillStep(tid, did, output); err != nil {
			return errResult(err.Error())
		}
	} else {
		if err := app.Scheduler.SubmitDecisionWithPayload(tid, did, choice, output); err != nil {
			return errResult(err.Error())
		}
	}

	// Attach human approval evidence to the task log if provided.
	// Non-blocking: logging failure does not fail the decision itself.
	if hasApproval && app.Logger != nil {
		evidenceText, _ := humanApproval["evidence_text"].(string)
		evidenceRef, _ := humanApproval["evidence_ref"].(string)
		enabled, _ := humanApproval["enabled"].(bool)
		app.Logger.Log(obs.EventHumanApprovalAttached, tid, "", map[string]any{
			"decision_id":   did,
			"choice":        choice,
			"enabled":       enabled,
			"evidence_text": evidenceText,
			"evidence_ref":  evidenceRef,
		})
	}
	// Build response. §4.2 (DELEGATION_HANDOVER_REDESIGN.md): after FulfillStep
	// succeeds in delegation mode, synchronously return the next batch of
	// delegation envelopes so the host can continue without an extra
	// wait_task poll. This reduces N-step round-trips from 1+2N to N+1.
	resp := map[string]any{
		"ok":             true,
		"human_approval": hasApproval,
	}
	if route.outputDropped {
		resp["output_ignored"] = true
	}
	if route.toFulfill && app.Registry != nil && app.Registry.System.DelegationMode {
		delegations, found := app.Scheduler.FirstDelegations(ctx, tid, delegationEnvelopeWait(app.Registry))
		if found {
			enrichDelegationViews(delegations, EffectiveTier(ctx, app), nil)
			resp["delegations"] = delegations
			resp["next_action"] = "submit_decision"
			resp["pending_decision_count"] = len(delegations)
		} else {
			resp["next_action"] = "get_task_status"
			resp["view_hint"] = "full"
		}
	}
	return jsonOK(resp)
}
