package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/pkg/observability"
)

// TestFMCBatchPrompt_ScrubsSecrets verifies that observability.Redact
// catches secrets in the exact prompt format used by RunFMCBatch. This is a
// unit test for the scrubbing at the FMC batch call site — the full
// RunFMCBatch flow requires an ExperienceStore with failure nodes to set up.
func TestFMCBatchPrompt_ScrubsSecrets(t *testing.T) {
	// observability.Redact catches key=value patterns like:
	//   api_key=..., password=..., token=..., secret=..., authorization=...
	secret := "sk-abcDef1234567890XYZ12"

	// RunFMCBatch prompt format (fmc_batch.go ~line 35)
	prompt := fmt.Sprintf(`Analyze this agent failure:
Task: %s
Capability: %s
Action attempted: %s
Error signal: %s

Assign a FailureMode and provide a Critique.
Output MUST be valid JSON: {"failure_mode": "...", "critique": "..."}`,
		"task with api_key="+secret,
		"test_cap",
		"action with token="+secret,
		"error: password="+secret,
	)

	scrubbed := observability.Redact(prompt)

	// The secret appears after key= patterns that Redact should catch.
	// Verify at least some instances are scrubbed.
	if !strings.Contains(scrubbed, "****") {
		t.Errorf("expected redacted values (****) in scrubbed FMC prompt, got: %s", scrubbed)
	}
	// Verify the raw secret doesn't appear after the known key patterns
	if strings.Contains(scrubbed, "api_key="+secret) {
		t.Errorf("secret leaked after api_key= in FMC prompt")
	}
	if strings.Contains(scrubbed, "token="+secret) {
		t.Errorf("secret leaked after token= in FMC prompt")
	}
	if strings.Contains(scrubbed, "password="+secret) {
		t.Errorf("secret leaked after password= in FMC prompt")
	}
}
