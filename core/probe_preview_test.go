package core

// This file contains two tests:
// 1. TestPreviewPrompt_MinimalConfig — a CI-runnable regression test that
//    verifies prompt assembly produces non-empty output with a minimal fixture.
// 2. TestProbePreviewPrompt — a manual diagnostic tool for debugging prompt
//    assembly with a real config (requires PROBE_CONFIG env var).

import (
	"os"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
)

// TestPreviewPrompt_MinimalConfig is the CI-runnable regression test for
// prompt assembly (audit T-2: was self-skipping with no CI coverage).
// It creates a minimal registry with a role that has an instruction, then
// verifies PreviewPrompt returns non-empty output containing the role's
// instruction text. If prompt assembly silently breaks (empty output), this
// test catches it without requiring any external config.
func TestPreviewPrompt_MinimalConfig(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"test_agent": {
				ID:             "test_agent",
				Name:           "Test Agent",
				BaseCapability: "general",
				Instruction:    "You are a test agent. Follow instructions carefully.",
			},
		},
	}
	logDir := t.TempDir()
	logger := mustNewLogger(t, logDir, &config.SystemSettings{})
	sp := NewSpawner(reg, nil, nil, logger, NewResourceLoader(), "")

	out, err := sp.PreviewPrompt("test_agent", "do a test task", map[string]any{})
	if err != nil {
		t.Fatalf("PreviewPrompt failed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("T-2 REGRESSION: PreviewPrompt returned empty output — prompt assembly is broken")
	}
	// Verify the role's instruction text appears in the assembled prompt.
	// This catches the case where assembly runs but drops the role's content.
	if !strings.Contains(out, "test agent") {
		t.Errorf("T-2 REGRESSION: PreviewPrompt output does not contain role instruction text. "+
			"Output (first 300 chars): %.300s", out)
	}
}

// TestProbePreviewPrompt is a manual diagnostic tool for debugging prompt
// assembly with a real config. It requires the PROBE_CONFIG env var to point
// to a config.json file. This test is NOT a regression test — it skips in CI.
// Use TestPreviewPrompt_MinimalConfig for CI coverage instead.
func TestProbePreviewPrompt(t *testing.T) {
	cfgPath := os.Getenv("PROBE_CONFIG")
	if cfgPath == "" {
		t.Skip("PROBE_CONFIG not set — use TestPreviewPrompt_MinimalConfig for CI coverage")
	}
	reg, err := config.NewRegistry(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	_lg, _ := obs.NewLogger(os.TempDir(), &reg.System)
	sp := NewSpawner(reg, nil, nil, _lg, NewResourceLoader(), "")
	out, err := sp.PreviewPrompt("customer_service_agent",
		"Handle this customer service conversation. The customer (user id noah_muller_9847) wants to book a flight from San Francisco to New York for three passengers.",
		map[string]any{})
	if err != nil {
		t.Fatalf("PreviewPrompt: %v", err)
	}
	t.Logf("assembled prompt length = %d", len(out))
	t.Logf("head 600: %.600s", out)
	if len(out) == 0 {
		t.Fatal("ERROR: assembled system prompt is empty — empty prompt root cause is in the assembly layer")
	}
}
