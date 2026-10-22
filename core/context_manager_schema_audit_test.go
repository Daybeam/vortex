package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// Regression for A10 (eval FINDINGS.md §A10, 2026-10-22): ContextManager.Audit
// counted only block text (countZone concatenates b.Text), so the MCP tool
// schemas the provider serializes into the tools parameter of every call were
// invisible to the budget judgment — AppWorld exposes 49 tools per task (453
// in full mode), all real tokens. n_ctx overflow was never flagged in
// advance partly because of this. After the fix the schemas are counted into
// OriginalTokens and carried on the uncompressible side of the Anti-Cliff
// math (Squeeze cannot touch them, same as the Protected Zone).

// schemaTool builds a ToolDefinition whose JSON-marshalled length is
// deterministic: the mock CountTokens is len/4, so tokenText-style sizing
// applies to the marshalled bytes.
func schemaTool(name, desc string) schemas.ToolDefinition {
	return schemas.ToolDefinition{
		Name:        name,
		Description: desc,
		InputSchema: map[string]any{"type": "object"},
	}
}

func TestAudit_CountsMCPServerSchemaTokens(t *testing.T) {
	// maxCtx=4000: plenty of headroom, so the decision stays PASS and the
	// counting itself can be asserted without tier interference.
	cm := NewContextManager(newAntiCliffRegistry(4000, 4000, 4000))
	p := &MockProvider{name: "anti"}

	tools := []schemas.ToolDefinition{
		schemaTool("venmo__show_profile", "Show the user's Venmo profile"),
		schemaTool("venmo__send_payment", "Send a payment to another user"),
	}
	req := &schemas.CompleteRequest{
		User: tokenText(10),
		MCPServers: []schemas.MCPServerDef{
			{Name: "venmo", URL: "http://x", Tools: tools},
		},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit returned error: %v", err)
	}
	if res.MCSchemaTokens <= 0 {
		t.Fatalf("A10 REGRESSION: MCPServers schema tokens not counted (MCSchemaTokens=%d)", res.MCSchemaTokens)
	}
	if res.OriginalTokens < res.MCSchemaTokens {
		t.Errorf("A10 REGRESSION: OriginalTokens (%d) must include the schema tokens (%d)",
			res.OriginalTokens, res.MCSchemaTokens)
	}
	t.Logf("[a10] MCSchemaTokens=%d OriginalTokens=%d decision=%v",
		res.MCSchemaTokens, res.OriginalTokens, res.Decision)
}

func TestAudit_NoMCPServersIsZeroAndUnchanged(t *testing.T) {
	// Control: requests without MCPServers must count exactly as before —
	// the A10 accounting changes nothing for schema-less requests.
	cm := NewContextManager(newAntiCliffRegistry(200, 200, 200))
	p := &MockProvider{name: "anti"}

	req := &schemas.CompleteRequest{
		ProtectedSystemBlocks: []schemas.ContentBlock{tokenBlock(20)},
		VolatileUserBlocks:    []schemas.ContentBlock{tokenBlock(30)},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit returned error: %v", err)
	}
	if res.MCSchemaTokens != 0 {
		t.Errorf("A10 control: MCSchemaTokens should be 0 without MCPServers, got %d", res.MCSchemaTokens)
	}
	if res.OriginalTokens != 50 {
		t.Errorf("A10 control: OriginalTokens should stay 50 (20 protected + 30 volatile), got %d", res.OriginalTokens)
	}
	if res.Decision != LevelNone {
		t.Errorf("A10 control: decision should stay LevelNone, got %v", res.Decision)
	}
}

func TestAudit_SchemaEatsCompressibleBudget(t *testing.T) {
	// The honest-budget claim: a big schema is uncompressible load, so a
	// volatile payload that USED to fit must now trip a compression tier.
	// maxCtx=400, effCtx=400, no reserve issues (reserve defaults large and
	// is skipped when it exceeds the window — see Audit 2.1).
	//
	// Layout: schema ≈ 30 tokens, volatile = 190 tokens.
	// Before A10: compressibleBudget = 400 - 0 (protected) = 400 → 190 fits → PASS.
	// After A10:  compressibleBudget = 400 - 30 (schema) = 370 → 190 still fits
	//             the Aggressive check (> budget) but… we size volatile ABOVE the
	//             remaining budget to force the tier change.
	cm := NewContextManager(newAntiCliffRegistry(400, 400, 400))
	p := &MockProvider{name: "anti"}

	bigTools := make([]schemas.ToolDefinition, 10)
	for i := range bigTools {
		bigTools[i] = schemaTool("venmo__tool_"+string(rune('a'+i)), "do something meaningful with the payment flow")
	}
	req := &schemas.CompleteRequest{
		VolatileUserBlocks: []schemas.ContentBlock{tokenBlock(360)},
		MCPServers:         []schemas.MCPServerDef{{Name: "venmo", URL: "http://x", Tools: bigTools}},
	}
	res, err := cm.Audit(context.Background(), p, req)
	if err != nil {
		t.Fatalf("Audit returned error: %v", err)
	}
	t.Logf("[a10-tier] MCSchemaTokens=%d OriginalTokens=%d decision=%v action=%s",
		res.MCSchemaTokens, res.OriginalTokens, res.Decision, res.ActionTaken)

	// 360 volatile tokens alone fit in effCtx=400 (LevelNone before A10).
	// With the schema subtracted from the budget, volatile must now exceed
	// the remaining compressible budget → at least LevelLight must fire.
	if res.Decision == LevelNone {
		t.Errorf("A10 REGRESSION: schema did not eat the compressible budget — decision stayed "+
			"LevelNone with MCSchemaTokens=%d and volatile=360 in effCtx=400", res.MCSchemaTokens)
	}
}
