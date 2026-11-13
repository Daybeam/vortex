package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// rolelessHub creates a hub with MCPs covering all mount policies and a
// name collision: both open_mcp and admin_mcp expose "search".
func rolelessHub() *core.ContextHub {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"open_mcp":    {ID: "open_mcp", MountPolicy: config.MountPolicyOpen, AvailableTools: []string{"search", "list"}},
			"admin_mcp":   {ID: "admin_mcp", MountPolicy: config.MountPolicyAdminOnly, AvailableTools: []string{"search", "admin_tool"}},
			"approv_mcp":  {ID: "approv_mcp", MountPolicy: config.MountPolicyApproval, AvailableTools: []string{"approved_tool"}},
			"default_mcp": {ID: "default_mcp", AvailableTools: []string{"unique_tool"}},
		},
		DynamicMCPs: map[string]*config.MCPDef{},
	}
	for _, m := range reg.MCPs {
		m.InitFilter()
	}
	return core.NewContextHub(reg, nil, nil)
}

func rolelessAdminCtx() context.Context {
	return context.WithValue(context.Background(), AuthTierKey, TierAdmin)
}

func rolelessPublicCtx() context.Context {
	return context.WithValue(context.Background(), AuthTierKey, TierPublic)
}

// --- buildAllMCPBindings ---

func TestBuildAllMCPBindings(t *testing.T) {
	hub := rolelessHub()
	bindings := buildAllMCPBindings(hub)
	if len(bindings) != 4 {
		t.Fatalf("expected 4 bindings, got %d", len(bindings))
	}
	ids := make(map[string]bool)
	for _, b := range bindings {
		ids[b.MCPID] = true
	}
	for _, expected := range []string{"open_mcp", "admin_mcp", "approv_mcp", "default_mcp"} {
		if !ids[expected] {
			t.Errorf("expected binding for %s", expected)
		}
	}
}

func TestBuildAllMCPBindings_SkipsDisabled(t *testing.T) {
	hub := rolelessHub()
	hub.Registry.MCPs["open_mcp"].Disabled = true
	bindings := buildAllMCPBindings(hub)
	for _, b := range bindings {
		if b.MCPID == "open_mcp" {
			t.Fatal("disabled MCP should not appear in bindings")
		}
	}
}

func TestBuildAllMCPBindings_IncludesDynamic(t *testing.T) {
	hub := rolelessHub()
	hub.Registry.Mu.Lock()
	hub.Registry.DynamicMCPs["dyn_mcp"] = &config.MCPDef{ID: "dyn_mcp", AvailableTools: []string{"dyn_tool"}}
	hub.Registry.Mu.Unlock()
	bindings := buildAllMCPBindings(hub)
	found := false
	for _, b := range bindings {
		if b.MCPID == "dyn_mcp" {
			found = true
		}
	}
	if !found {
		t.Fatal("dynamic MCP should appear in bindings")
	}
}

// --- findToolCandidates ---

func TestFindToolCandidates(t *testing.T) {
	hub := rolelessHub()
	// "search" is on both open_mcp and admin_mcp
	candidates := findToolCandidates("search", hub)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates for 'search', got %d: %v", len(candidates), candidates)
	}
	// "unique_tool" is only on default_mcp
	candidates = findToolCandidates("unique_tool", hub)
	if len(candidates) != 1 || candidates[0] != "default_mcp" {
		t.Fatalf("expected 1 candidate 'default_mcp', got %v", candidates)
	}
	// "nonexistent" → 0 candidates
	candidates = findToolCandidates("nonexistent", hub)
	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates, got %v", candidates)
	}
}

func TestFindToolCandidates_DisabledMCP(t *testing.T) {
	hub := rolelessHub()
	hub.Registry.MCPs["open_mcp"].Disabled = true
	candidates := findToolCandidates("search", hub)
	if len(candidates) != 1 || candidates[0] != "admin_mcp" {
		t.Fatalf("expected only admin_mcp after disabling open_mcp, got %v", candidates)
	}
}

func TestFindToolCandidates_DisabledTool(t *testing.T) {
	hub := rolelessHub()
	hub.Registry.MCPs["open_mcp"].DisabledTools = []string{"search"}
	candidates := findToolCandidates("search", hub)
	if len(candidates) != 1 || candidates[0] != "admin_mcp" {
		t.Fatalf("expected only admin_mcp after disabling search on open_mcp, got %v", candidates)
	}
}

// --- checkRolelessGate ---

func TestCheckRolelessGate_Open(t *testing.T) {
	hub := rolelessHub()
	if err := checkRolelessGate(hub, "open_mcp", TierAdmin); err != nil {
		t.Fatalf("open MCP should pass for admin: %v", err)
	}
	if err := checkRolelessGate(hub, "open_mcp", TierPublic); err != nil {
		t.Fatalf("open MCP should pass for public: %v", err)
	}
}

func TestCheckRolelessGate_AdminOnly(t *testing.T) {
	hub := rolelessHub()
	if err := checkRolelessGate(hub, "admin_mcp", TierAdmin); err != nil {
		t.Fatalf("admin should pass admin_only: %v", err)
	}
	if err := checkRolelessGate(hub, "admin_mcp", TierPublic); err == nil {
		t.Fatal("public tier should be rejected by admin_only")
	}
	// Default (empty policy) → admin_only
	if err := checkRolelessGate(hub, "default_mcp", TierPublic); err == nil {
		t.Fatal("public tier should be rejected by default admin_only")
	}
}

func TestCheckRolelessGate_Approval(t *testing.T) {
	hub := rolelessHub()
	// Admin passes approval without record
	if err := checkRolelessGate(hub, "approv_mcp", TierAdmin); err != nil {
		t.Fatalf("admin should pass approval: %v", err)
	}
	// Non-admin without approval → rejected
	if err := checkRolelessGate(hub, "approv_mcp", TierPublic); err == nil {
		t.Fatal("public tier without approval should be rejected")
	}
	// Non-admin WITH approval → passes
	hub.Graph = &schemas.TaskGraph{
		ApprovedMCPMounts: map[string]*schemas.ApprovalRecord{
			"approv_mcp": {Approved: true},
		},
	}
	if err := checkRolelessGate(hub, "approv_mcp", TierPublic); err != nil {
		t.Fatalf("public tier with approval should pass: %v", err)
	}
}

func TestCheckRolelessGate_ServerRiskyDynamic(t *testing.T) {
	hub := rolelessHub()
	// Add a dynamic MCP that is server-risky (local command without sandbox)
	hub.Registry.Mu.Lock()
	hub.Registry.DynamicMCPs["risky_dyn"] = &config.MCPDef{ID: "risky_dyn", Command: "echo", Sandboxed: false}
	hub.Registry.Mu.Unlock()
	// Even admin is rejected for server-risky dynamic MCP in roleless path
	if err := checkRolelessGate(hub, "risky_dyn", TierAdmin); err == nil {
		t.Fatal("server-risky dynamic MCP should be rejected even for admin in roleless path")
	}
}

func TestCheckRolelessGate_NonRiskyDynamic(t *testing.T) {
	hub := rolelessHub()
	// Add a dynamic MCP that is NOT server-risky (URL-based, public, no credential)
	hub.Registry.Mu.Lock()
	hub.Registry.DynamicMCPs["safe_dyn"] = &config.MCPDef{ID: "safe_dyn", URL: "https://api.example.com", MountPolicy: config.MountPolicyOpen}
	hub.Registry.Mu.Unlock()
	// Admin should pass
	if err := checkRolelessGate(hub, "safe_dyn", TierAdmin); err != nil {
		t.Fatalf("non-risky dynamic MCP should pass for admin: %v", err)
	}
	// Public should pass (open policy)
	if err := checkRolelessGate(hub, "safe_dyn", TierPublic); err != nil {
		t.Fatalf("non-risky dynamic MCP with open policy should pass for public: %v", err)
	}
}

// --- resolveRolelessMCP ---

func TestResolveRolelessMCP_OpenSuccess(t *testing.T) {
	hub := rolelessHub()
	app := &App{Registry: hub.Registry}
	spawner := &core.Spawner{}
	mcpID, err := resolveRolelessMCP(rolelessAdminCtx(), app, hub, spawner, "list", "")
	if err != nil {
		t.Fatalf("expected success for 'list' on open_mcp, got: %v", err)
	}
	if mcpID != "open_mcp" {
		t.Fatalf("expected open_mcp, got %s", mcpID)
	}
}

func TestResolveRolelessMCP_OpenSuccessForPublic(t *testing.T) {
	hub := rolelessHub()
	app := &App{Registry: hub.Registry}
	spawner := &core.Spawner{}
	// "list" is only on open_mcp (open policy) → public tier should pass
	mcpID, err := resolveRolelessMCP(rolelessPublicCtx(), app, hub, spawner, "list", "")
	if err != nil {
		t.Fatalf("expected success for 'list' on open_mcp for public tier, got: %v", err)
	}
	if mcpID != "open_mcp" {
		t.Fatalf("expected open_mcp, got %s", mcpID)
	}
}

func TestResolveRolelessMCP_AdminOnlyRejectedForNonAdmin(t *testing.T) {
	hub := rolelessHub()
	app := &App{Registry: hub.Registry}
	spawner := &core.Spawner{}
	_, err := resolveRolelessMCP(rolelessPublicCtx(), app, hub, spawner, "admin_tool", "")
	if err == nil {
		t.Fatal("non-admin should be rejected by admin_only MCP")
	}
	if !strings.Contains(err.Error(), "admin") {
		t.Fatalf("expected admin tier requirement error, got: %v", err)
	}
}

func TestResolveRolelessMCP_Disambiguation(t *testing.T) {
	hub := rolelessHub()
	app := &App{Registry: hub.Registry}
	spawner := &core.Spawner{}
	_, err := resolveRolelessMCP(rolelessAdminCtx(), app, hub, spawner, "search", "")
	if err == nil {
		t.Fatal("ambiguous tool 'search' should return disambiguation error")
	}
	if !strings.Contains(err.Error(), "multiple MCPs") {
		t.Fatalf("expected disambiguation error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "mcp_id") {
		t.Fatalf("error should mention mcp_id for disambiguation, got: %v", err)
	}
	// Error should list both candidates with their mount policies
	if !strings.Contains(err.Error(), "open_mcp") || !strings.Contains(err.Error(), "admin_mcp") {
		t.Fatalf("error should list both candidate MCPs, got: %v", err)
	}
}

func TestResolveRolelessMCP_McpIDEscape(t *testing.T) {
	hub := rolelessHub()
	app := &App{Registry: hub.Registry}
	spawner := &core.Spawner{}
	// Use mcp_id to disambiguate "search"
	mcpID, err := resolveRolelessMCP(rolelessAdminCtx(), app, hub, spawner, "search", "open_mcp")
	if err != nil {
		t.Fatalf("mcp_id escape should succeed, got: %v", err)
	}
	if mcpID != "open_mcp" {
		t.Fatalf("expected open_mcp, got %s", mcpID)
	}
}

func TestResolveRolelessMCP_McpIDNotFound(t *testing.T) {
	hub := rolelessHub()
	app := &App{Registry: hub.Registry}
	spawner := &core.Spawner{}
	_, err := resolveRolelessMCP(rolelessAdminCtx(), app, hub, spawner, "search", "nonexistent_mcp")
	if err == nil {
		t.Fatal("nonexistent mcp_id should return error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestResolveRolelessMCP_McpIDDisabledTool(t *testing.T) {
	hub := rolelessHub()
	hub.Registry.MCPs["open_mcp"].DisabledTools = []string{"search"}
	app := &App{Registry: hub.Registry}
	spawner := &core.Spawner{}
	_, err := resolveRolelessMCP(rolelessAdminCtx(), app, hub, spawner, "search", "open_mcp")
	if err == nil {
		t.Fatal("disabled tool on specified mcp_id should return error")
	}
}

func TestResolveRolelessMCP_NotFound(t *testing.T) {
	// Empty hub: no MCPs at all
	reg := &config.Registry{MCPs: map[string]*config.MCPDef{}}
	hub := core.NewContextHub(reg, nil, nil)
	app := &App{Registry: reg}
	spawner := &core.Spawner{}
	_, err := resolveRolelessMCP(rolelessAdminCtx(), app, hub, spawner, "nonexistent_tool", "")
	if err == nil {
		t.Fatal("nonexistent tool should return error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestResolveRolelessMCP_ServerRiskyDynamic(t *testing.T) {
	hub := rolelessHub()
	// Add a dynamic MCP that is server-risky with a tool
	riskyMcp := &config.MCPDef{
		ID: "risky_dyn", Command: "echo", Sandboxed: false,
		AvailableTools: []string{"risky_tool"},
	}
	riskyMcp.InitFilter()
	hub.Registry.Mu.Lock()
	hub.Registry.DynamicMCPs["risky_dyn"] = riskyMcp
	hub.Registry.Mu.Unlock()

	app := &App{Registry: hub.Registry}
	spawner := &core.Spawner{}
	_, err := resolveRolelessMCP(rolelessAdminCtx(), app, hub, spawner, "risky_tool", "")
	if err == nil {
		t.Fatal("server-risky dynamic MCP should be rejected in roleless path")
	}
	if !strings.Contains(err.Error(), "server-risky") {
		t.Fatalf("expected server-risky error, got: %v", err)
	}
}

func TestResolveRolelessMCP_ApprovalWithGraph(t *testing.T) {
	hub := rolelessHub()
	hub.Graph = &schemas.TaskGraph{
		ApprovedMCPMounts: map[string]*schemas.ApprovalRecord{
			"approv_mcp": {Approved: true},
		},
	}
	app := &App{Registry: hub.Registry}
	spawner := &core.Spawner{}
	// "approved_tool" is on approv_mcp; with approval record, public tier should pass
	mcpID, err := resolveRolelessMCP(rolelessPublicCtx(), app, hub, spawner, "approved_tool", "")
	if err != nil {
		t.Fatalf("approved MCP should pass for public tier with approval record: %v", err)
	}
	if mcpID != "approv_mcp" {
		t.Fatalf("expected approv_mcp, got %s", mcpID)
	}
}

func TestResolveRolelessMCP_ApprovalWithoutGraph(t *testing.T) {
	hub := rolelessHub()
	app := &App{Registry: hub.Registry}
	spawner := &core.Spawner{}
	// "approved_tool" is on approv_mcp; without approval record, public tier should be rejected
	_, err := resolveRolelessMCP(rolelessPublicCtx(), app, hub, spawner, "approved_tool", "")
	if err == nil {
		t.Fatal("approval MCP should reject public tier without approval record")
	}
}
