package core

import (
	"context"
	"strings"
	"testing"
)

func TestGatewayIntegrationWithExecutor(t *testing.T) {
	exec := NewControlledExecutor()
	pyScript := `
import sys
for i in range(500):
    if i == 250:
        print('FATAL: simulated crash at line 250')
    else:
        print(f'Line normal {i} ' + 'A' * 40)
`
	res, err := exec.Run(context.Background(), "python3", []string{"-c", pyScript}, "", nil)
	if err != nil {
		t.Skipf("python3 not available or failed: %v", err)
		return
	}

	gated := AdaptiveOutputResult(res.Stdout, "python3")
	gatedStr := string(gated)

	if !strings.Contains(gatedStr, "TRUNCATED") && !strings.Contains(gatedStr, "GATEWAY") {
		t.Errorf("expected output to be gated or truncated, got len=%d", len(gatedStr))
	}
	if !strings.Contains(gatedStr, "FATAL: simulated crash") {
		t.Errorf("expected critical error keyword to be preserved, got: %s", gatedStr)
	}
}
