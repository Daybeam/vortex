package skeleton

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestGatewayIntegrationWithExecutor(t *testing.T) {
	// Originally used core's NewControlledExecutor; replaced with plain
	// os/exec at the P2.5 extraction so the package stays self-contained.
	pyScript := `
import sys
for i in range(500):
    if i == 250:
        print('FATAL: simulated crash at line 250')
    else:
        print(f'Line normal {i} ' + 'A' * 40)
`
	stdout, err := exec.CommandContext(context.Background(), "python3", "-c", pyScript).Output()
	if err != nil {
		t.Skipf("python3 not available or failed: %v", err)
		return
	}

	gated := AdaptiveOutputResult(stdout, "python3", "")
	gatedStr := string(gated)

	if !strings.Contains(gatedStr, "TRUNCATED") && !strings.Contains(gatedStr, "GATEWAY") {
		t.Errorf("expected output to be gated or truncated, got len=%d", len(gatedStr))
	}
	if !strings.Contains(gatedStr, "FATAL: simulated crash") {
		t.Errorf("expected critical error keyword to be preserved, got: %s", gatedStr)
	}
}

// TestGatewayIntegration_EmptyOutput verifies that empty command output
// doesn't panic and returns empty bytes (not a header or marker).
func TestGatewayIntegration_EmptyOutput(t *testing.T) {
	got := AdaptiveOutputResult([]byte{}, "test", "")
	if len(got) != 0 {
		t.Errorf("empty input should return empty, got %d bytes: %q", len(got), got)
	}
}

// TestGatewayIntegration_NoErrorKeywords verifies that output with no error
// keywords is still truncated (via mid-section sampling, not error extraction).
func TestGatewayIntegration_NoErrorKeywords(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 250; i++ {
		sb.WriteString("processing item " + itoa(i) + " completed successfully\n")
	}
	data := []byte(sb.String())
	if len(data) < 8*1024 {
		t.Fatalf("test data too small (%d bytes)", len(data))
	}

	got := string(AdaptiveOutputResult(data, "test", ""))
	// Should be truncated (not passed through verbatim).
	if len(got) >= len(data) {
		t.Errorf("output with no error keywords should still be truncated: got %d bytes >= input %d bytes", len(got), len(data))
	}
	// Should contain mid-section samples (Gap 4 feature).
	if !strings.Contains(got, "Middle samples") {
		t.Errorf("expected mid-section samples for output with no error keywords, got:\n%s", got[:200])
	}
}
