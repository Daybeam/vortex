package tools

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// ──────────────────────────────────────────────────────────────────────────────
// Regression tests for DELEGATION_HANDOVER_REDESIGN.md §12.6 L2 (P0):
// orchestrator_run_command and orchestrator_get_logs outputs were not passed
// through RedactEgress, so secrets in command output (e.g. `cat .env`) or in
// logged events reached the host model's context window verbatim.
//
// L2a (run_command): redactAndGateOutput now strips secrets before summarizing.
// L2b (get_logs):    jsonOKRedacted now strips secrets from the serialized JSON.
// Before fix: both helpers did not exist / raw output was returned.
// After fix:  secrets are replaced with [REDACTED] / [SECRET REDACTED] markers.
// ──────────────────────────────────────────────────────────────────────────────

func TestRedactAndGateOutput_StripsSecretsFromCommandOutput(t *testing.T) {
	// Realistic `cat .env` output — the exact attack vector from §12.2 L2.
	envDump := []byte("ANTHROPIC_API_KEY=sk-ant-supersecret1234567890abcdef\n" +
		"OPENAI_API_KEY=sk-proj-abcdefghij1234567890XYZ\n" +
		"DATABASE_URL=postgres://user:pass@example.org:5432/db\n")

	gatedOut, _ := redactAndGateOutput(envDump, nil, "cat")

	got := string(gatedOut)
	for _, leak := range []string{
		"sk-ant-supersecret1234567890abcdef",
		"sk-proj-abcdefghij1234567890XYZ",
	} {
		if strings.Contains(got, leak) {
			t.Errorf("secret leaked through run_command output: %q present in %q", leak, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") && !strings.Contains(got, "[SECRET REDACTED]") {
		t.Errorf("expected a redaction marker in output, got: %q", got)
	}
}

func TestRedactAndGateOutput_PreservesNonSecretOutput(t *testing.T) {
	// Invariant I4: non-secret output must pass through unchanged (modulo
	// summarization). This guards against over-redaction breaking normal use.
	out := []byte("Build succeeded. 42 tests passed.\n")
	gatedOut, gatedErr := redactAndGateOutput(out, []byte(""), "go")
	if !strings.Contains(string(gatedOut), "42 tests passed") {
		t.Errorf("non-secret output was altered: %q", string(gatedOut))
	}
	if string(gatedErr) != "" {
		t.Errorf("expected empty stderr, got: %q", string(gatedErr))
	}
}

func TestJSONOKRedacted_StripsSecretsFromLogEvents(t *testing.T) {
	// get_logs returns []map[string]any; jsonOKRedacted marshals then redacts.
	events := []map[string]any{
		{
			"event":  "tool_call",
			"tool":   "run_command",
			"stdout": "ANTHROPIC_API_KEY=sk-ant-leakedsecret1234567890ab",
		},
		{
			"event": "tool_call",
			"args":  []string{"cat", ".env"},
		},
	}

	result, err := jsonOKRedacted(events)
	if err != nil {
		t.Fatalf("jsonOKRedacted returned error: %v", err)
	}

	text := result.Content[0].(mcp.TextContent).Text
	if strings.Contains(text, "sk-ant-leakedsecret1234567890ab") {
		t.Errorf("secret leaked through get_logs output: %q", text)
	}
	if !strings.Contains(text, "[REDACTED]") && !strings.Contains(text, "[SECRET REDACTED]") {
		t.Errorf("expected a redaction marker in get_logs JSON, got: %q", text)
	}
}
