package providers

import (
	"errors"
	"strings"
	"testing"
)

// The bodies are the real shapes seen on the wire, not paraphrases:
//
//   - budgetBody: harness-bench's tools/model_budget_proxy.py, HTTP 400, sent
//     once the run's provider-request allowance is spent.
//   - contextBody: llama.cpp's context-overflow 400. Must stay a
//     BadRequestError — core/spawner_utils.go's IsContextLimitError detects it
//     by body wording ("context size", "n_ctx") and the step-level
//     context-overflow recovery keys off that.
//   - schemaBody: an ordinary malformed request.
const (
	budgetBody  = `{"error":{"type":"max_agent_turns_exceeded","message":"HarnessBench maximum provider request budget of 40 was reached"}}`
	contextBody = `{"error":{"code":400,"message":"request (17164 tokens) exceeds the available context size (16384 tokens)","type":"exceed_context_size_error","n_prompt_tokens":17164,"n_ctx":16384}}`
	schemaBody  = `{"error":{"type":"invalid_request_error","message":"messages[0].role must be one of user, assistant"}}`
)

func TestClassifyBadRequest(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		budget bool
	}{
		{"budget refusal", budgetBody, true},
		{"budget wording without JSON envelope", "maximum provider request budget of 40 was reached", true},
		{"budget marker upper case", `{"error":{"type":"MAX_AGENT_TURNS_EXCEEDED"}}`, true},
		{"context overflow stays bad request", contextBody, false},
		{"schema rejection", schemaBody, false},
		{"empty body", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyBadRequest([]byte(tc.body))

			var budgetErr *BudgetExhaustedError
			var badReqErr *BadRequestError
			wantBadReq := !tc.budget
			if got := errors.As(err, &budgetErr); got != tc.budget {
				t.Fatalf("BudgetExhaustedError = %v, want %v (err: %v)", got, tc.budget, err)
			}
			if got := errors.As(err, &badReqErr); got != wantBadReq {
				t.Fatalf("BadRequestError = %v, want %v (err: %v)", got, wantBadReq, err)
			}
			// The body must survive verbatim: analysis reads it, and
			// SanitizeError downstream needs the real text.
			if !strings.Contains(err.Error(), tc.body) && tc.body != "" {
				t.Fatalf("body not preserved in message: %q", err.Error())
			}
		})
	}
}

// The whole point of D3: the text must not claim the request was malformed.
func TestBudgetExhaustedError_DoesNotReadAsBadRequest(t *testing.T) {
	err := classifyBadRequest([]byte(budgetBody))
	if strings.Contains(err.Error(), "bad request:") {
		t.Fatalf("budget refusal still reads as a malformed request: %q", err.Error())
	}
	if !strings.Contains(err.Error(), BudgetExhaustedMarker) {
		t.Fatalf("flattened-form marker missing from %q", err.Error())
	}
}

// The DAG path classifies step.LastError by text after the typed error is gone
// (scheduler_dag_exec.go's end-of-step re-classification), so the marker in the
// message has to be what classifyBadRequest would have said.
func TestBudgetExhaustedError_MarkerMatchesWireBody(t *testing.T) {
	if !strings.Contains(budgetBody, "max_agent_turns_exceeded") {
		t.Fatalf("fixture drifted: no wire marker in %q", budgetBody)
	}
	if !strings.Contains(budgetBody, "maximum provider request budget") {
		t.Fatalf("fixture drifted: no human marker in %q", budgetBody)
	}
}
