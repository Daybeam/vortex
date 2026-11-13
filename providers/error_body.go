package providers

import "strings"

// budgetExhaustedMarkers are the wire phrases a gateway uses when it refuses a
// request because the caller's allowance is gone, as opposed to the request
// being malformed. Both are matched case-insensitively against the raw 400
// body, because that body is the only signal the wire carries — the status
// code is 400 either way.
//
//   - max_agent_turns_exceeded: harness-bench's tools/model_budget_proxy.py
//     sends {"error":{"type":"max_agent_turns_exceeded","message":"HarnessBench
//     maximum provider request budget of N was reached"}} with HTTP 400.
//   - maximum provider request budget: the same message as plain text, so a
//     body that lost its JSON envelope (or a different proxy reusing the
//     wording) still classifies.
var budgetExhaustedMarkers = []string{
	"max_agent_turns_exceeded",
	"maximum provider request budget",
}

// classifyBadRequest turns a 400 response body into the narrowest honest error
// type. A budget refusal is NOT a malformed request and must not share its
// type: see BudgetExhaustedError for the two behaviours that depended on the
// distinction. Everything else — including llama.cpp's context-overflow 400s,
// which core/spawner_utils.go detects by body wording — stays a
// BadRequestError so existing handling is untouched.
func classifyBadRequest(body []byte) error {
	lower := strings.ToLower(string(body))
	for _, marker := range budgetExhaustedMarkers {
		if strings.Contains(lower, marker) {
			return &BudgetExhaustedError{Msg: string(body)}
		}
	}
	return &BadRequestError{Msg: string(body)}
}
