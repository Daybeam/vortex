package observability

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactValue_Containers is the regression test for G10 (2026-10-31):
// RedactMap only walked map[string]any and string values, so a credential
// nested in a list, a map[string]string, or a []map[string]any reached the
// log file and the telemetry endpoint unmasked. The audit found this on the
// telemetry path (typed TaskPattern/StepSequence values were never inspected)
// and on log details carrying tool-call lists.
func TestRedactValue_Containers(t *testing.T) {
	in := map[string]any{
		"calls": []any{
			map[string]any{"api_key": "supersecretvalue"},
			map[string]any{"note": "token=abcdef123456"},
		},
		"headers": map[string]string{
			"Authorization": "Bearer abcdef123456",
			"Accept":        "application/json",
		},
		"labels": []string{"password=supersecret", "harmless"},
		"steps": []map[string]any{
			{"secret": "abcdef123456"},
		},
		"prompt_tokens": 42,
	}

	out := RedactMap(in)

	rendered, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal redacted map: %v", err)
	}
	body := string(rendered)

	for _, leaked := range []string{
		"supersecretvalue",
		"abcdef123456",
		"Bearer abcdef123456",
		"password=supersecret",
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("secret %q survived redaction: %s", leaked, body)
		}
	}
	if !strings.Contains(body, "supe****") {
		t.Errorf("expected masked api_key with prefix, got %s", body)
	}
	if out["prompt_tokens"] != 42 {
		t.Errorf("non-secret numeric key was altered: %v", out["prompt_tokens"])
	}
	if got := out["labels"].([]string)[1]; got != "harmless" {
		t.Errorf("harmless label was altered: %q", got)
	}
}
