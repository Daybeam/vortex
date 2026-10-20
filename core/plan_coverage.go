package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

// plan_coverage.go — does the plan actually do what was asked?
//
// The planner is the only component that decides *what work exists*. If it plans
// the preparation and stops, nothing downstream can recover: the executor sees
// only its step's task text, routing derives its tool surface from that text,
// and the run then executes perfectly and scores zero.
//
// Measured 2026-10-19 (AppWorld 4fab96f_3, gemini-3-flash). The request was
// "…send a reminder for each pending request older than 50 days…". Three runs,
// three different plans:
//
//	q: 5 steps including "send a reminder using venmo__remind_payment_request"  -> 9 tools
//	r: prose including "send a reminder" and "mark the task as complete"       -> 9 tools
//	s: 4 steps ending "Save … to venmo_data.json"                              -> 8 tools
//
// Run s advertised exactly the 8 tool names its own step text mentioned. It
// could not have sent a reminder, because sending was never planned. The check
// below turns that silent zero into a planner retry.

// actionVerbs are user-request verbs that imply a state change in some app. If
// the request uses one of these, a plan made only of reads has not finished the
// job. Word-boundary matched, so "reminder" counts (remind + er) and "denied"
// does not trip "deny".
var actionVerbs = []string{
	"send", "remind", "create", "add", "cancel", "update", "delete", "remove",
	"mark", "pay", "transfer", "book", "schedule", "invite", "post", "archive",
	"move", "complete", "submit", "upload", "comment", "like", "follow",
}

// authToolKeywords mark tools that authenticate but change nothing the user
// asked for. Logging in is a precondition, not the requested effect — counting
// it as "the action" would let a read-only plan pass this check, which is
// exactly the shape that failed (venmo__login was the only non-read tool in the
// 8-tool surface of run s).
var authToolKeywords = []string{"login", "logout", "signin", "sign_in", "authenticate", "auth", "token"}

// isActionTool reports whether a tool can produce the effect a request asked for.
func isActionTool(name string) bool {
	if isReadTool(name) {
		return false
	}
	lower := strings.ToLower(name)
	if i := strings.LastIndex(lower, toolNamespaceSeparator); i >= 0 {
		lower = lower[i+len(toolNamespaceSeparator):]
	}
	for _, kw := range authToolKeywords {
		if strings.Contains(lower, kw) {
			return false
		}
	}
	return true
}

// requestNeedsAction reports whether the user asked for a state change, and
// which verb said so.
func requestNeedsAction(task string) (string, bool) {
	lower := strings.ToLower(task)
	for _, v := range actionVerbs {
		idx := 0
		for {
			i := strings.Index(lower[idx:], v)
			if i < 0 {
				break
			}
			start := idx + i
			end := start + len(v)
			if isWordBoundary(lower, start, end) {
				return v, true
			}
			idx = start + 1
		}
	}
	return "", false
}

func isWordBoundary(s string, start, end int) bool {
	alnum := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
	}
	if start > 0 && alnum(s[start-1]) {
		return false
	}
	if end < len(s) && alnum(s[end]) {
		return false
	}
	return true
}

// planCoverageGap returns a non-empty reason when the request asks for an effect
// that no step can produce. An empty return means "no gap detectable".
//
// Deliberately conservative. It only judges plans that declare tools: a planner
// that omits allowed_tools gets the benefit of the doubt, because rejecting on
// absence would fail the exact plans that were already accepted by
// ParsePlanDAG and would punish a convention rather than a mistake.
func planCoverageGap(userTask string, steps []schemas.StepInput) string {
	verb, needs := requestNeedsAction(userTask)
	if !needs {
		return ""
	}
	declared := 0
	for _, st := range steps {
		for _, tools := range st.AdditionalToolAllowlists {
			for _, t := range tools {
				declared++
				if isActionTool(t) {
					return ""
				}
			}
		}
	}
	if declared == 0 {
		return ""
	}
	return fmt.Sprintf(
		"plan has no action step: the request asks to %q but every one of the %d declared tools is read-only or a login. "+
			"Add the step that performs the effect (and the tool that performs it), or the run will gather data and stop.",
		verb, declared)
}
