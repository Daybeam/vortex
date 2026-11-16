package config

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestRegisterDynamicMCP(t *testing.T) {
	r := &Registry{
		DynamicMCPs: make(map[string]*MCPDef),
	}
	mcp := &MCPDef{ID: "dyn1"}
	r.RegisterDynamicMCP(mcp)
	if r.GetMCP("dyn1") != mcp {
		t.Error("expected to find dynamic MCP after registration")
	}
	r.UnregisterDynamicMCP("dyn1")
	if r.GetMCP("dyn1") != nil {
		t.Error("expected nil after unregister")
	}
}

func TestGetMCP_FallbackToStatic(t *testing.T) {
	r := &Registry{
		MCPs:       map[string]*MCPDef{"static1": {ID: "static1"}},
		DynamicMCPs: make(map[string]*MCPDef),
	}
	if r.GetMCP("static1") == nil {
		t.Error("expected to find static MCP")
	}
	if r.GetMCP("nonexistent") != nil {
		t.Error("expected nil for nonexistent MCP")
	}
}

func TestIsModelConfigured(t *testing.T) {
	r := &Registry{
		Providers: map[string]*ProviderConfig{
			"p1": {Model: "gpt-4o"},
			"p2": {Model: "claude-3"},
		},
	}
	if !r.IsModelConfigured("gpt-4o") {
		t.Error("expected gpt-4o to be configured")
	}
	if !r.IsModelConfigured("claude-3") {
		t.Error("expected claude-3 to be configured")
	}
	if r.IsModelConfigured("unknown-model") {
		t.Error("expected unknown-model to NOT be configured")
	}
}

func TestEstimateTokens(t *testing.T) {
	r := &Registry{
		Skills: map[string]*Skill{
			"s1": {TokenEstimate: 100},
			"s2": {TokenEstimate: 200},
		},
	}
	total := r.EstimateTokens([]string{"s1", "s2", "s3"})
	if total != 300 {
		t.Errorf("EstimateTokens = %d, want 300", total)
	}
}

func TestRecordFailure_CircuitBreaker(t *testing.T) {
	r := &Registry{}
	for i := 0; i < 4; i++ {
		r.RecordFailure("p1")
	}
	if !r.IsHealthy("p1") {
		t.Error("expected healthy before 5 failures")
	}
	r.RecordFailure("p1")
	if r.IsHealthy("p1") {
		t.Error("expected unhealthy after 5 failures (circuit open)")
	}
}

func TestIsHealthy_NoHealthData(t *testing.T) {
	r := &Registry{}
	if !r.IsHealthy("p1") {
		t.Error("expected healthy when no health data")
	}
}

func TestIsHealthy_CircuitOpened(t *testing.T) {
	r := &Registry{}
	for i := 0; i < 5; i++ {
		r.RecordFailure("p1")
	}
	if r.IsHealthy("p1") {
		t.Error("expected unhealthy after 5 failures (circuit open)")
	}
}

func TestProviderConfig_IsWithinValidity(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)

	p := &ProviderConfig{ValidFrom: &from, ValidUntil: &until}
	if !p.IsWithinValidity(now) {
		t.Error("expected within validity")
	}

	before := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	if p.IsWithinValidity(before) {
		t.Error("expected NOT within validity (before ValidFrom)")
	}

	after := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	if p.IsWithinValidity(after) {
		t.Error("expected NOT within validity (after ValidUntil)")
	}

	p = &ProviderConfig{}
	if !p.IsWithinValidity(now) {
		t.Error("expected within validity for nil bounds")
	}
}

func TestProviderInstance_IsWithinValidity(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	parentFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	parentUntil := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	parent := &ProviderConfig{ValidFrom: &parentFrom, ValidUntil: &parentUntil}

	inst := &ProviderInstance{}
	if !inst.IsWithinValidity(now, parent) {
		t.Error("expected within parent validity when instance has no bounds")
	}
	if !inst.IsWithinValidity(now, nil) {
		t.Error("expected within validity for nil bounds and nil parent")
	}

	instFrom := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	instUntil := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	inst = &ProviderInstance{ValidFrom: &instFrom, ValidUntil: &instUntil}
	if !inst.IsWithinValidity(now, parent) {
		t.Error("expected within instance validity")
	}

	beforeInst := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if inst.IsWithinValidity(beforeInst, parent) {
		t.Error("expected NOT within instance validity (before ValidFrom)")
	}
}

func TestRole_BoundMCPIDs(t *testing.T) {
	r := &Role{
		BoundMCPBindings: []MCPBinding{
			{MCPID: "mcp1"},
			{MCPID: "mcp2"},
		},
	}
	ids := r.BoundMCPIDs()
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs, got %d", len(ids))
	}
	if ids[0] != "mcp1" || ids[1] != "mcp2" {
		t.Errorf("IDs = %v, want [mcp1 mcp2]", ids)
	}
}

func TestRole_GetBinding(t *testing.T) {
	r := &Role{
		BoundMCPBindings: []MCPBinding{
			{MCPID: "mcp1", AllowedTools: []string{"tool_a"}},
			{MCPID: "mcp2"},
		},
	}
	b := r.GetBinding("mcp1")
	if b == nil {
		t.Fatal("expected binding for mcp1")
	}
	if b.MCPID != "mcp1" {
		t.Errorf("MCPID = %q, want mcp1", b.MCPID)
	}
	if r.GetBinding("nonexistent") != nil {
		t.Error("expected nil for nonexistent binding")
	}
}

func TestMCPBinding_IsRestricted(t *testing.T) {
	b := &MCPBinding{AllowedTools: []string{"tool_a"}}
	if !b.IsRestricted() {
		t.Error("expected restricted when AllowedTools is non-empty")
	}
	b = &MCPBinding{}
	if b.IsRestricted() {
		t.Error("expected NOT restricted when AllowedTools is empty")
	}
}

func TestSystemSettings_OutputDirOrDefault(t *testing.T) {
	s := &SystemSettings{OutputDir: "/custom/output"}
	if s.OutputDirOrDefault() != "/custom/output" {
		t.Errorf("got %q, want /custom/output", s.OutputDirOrDefault())
	}
	s = &SystemSettings{}
	if s.OutputDirOrDefault() != "outputs" {
		t.Errorf("got %q, want outputs", s.OutputDirOrDefault())
	}
	var nilSettings *SystemSettings
	if nilSettings.OutputDirOrDefault() != "outputs" {
		t.Errorf("got %q, want outputs for nil", nilSettings.OutputDirOrDefault())
	}
}

func TestLoadAuthConfig_Defaults(t *testing.T) {
	os.Unsetenv("VORTEX_AUTH_MODE")
	os.Unsetenv("VORTEX_ADMIN_KEY")
	os.Unsetenv("VORTEX_PUBLIC_KEY")
	os.Unsetenv("VORTEX_OAUTH_ISSUER")
	os.Unsetenv("VORTEX_OAUTH_SIGNING_KEY")
	os.Unsetenv("VORTEX_OAUTH_DCR_ENABLED")
	os.Unsetenv("VORTEX_OAUTH_FALLBACK_APIKEY")

	cfg := LoadAuthConfig()
	if cfg.Mode != "none" {
		t.Errorf("Mode = %q, want none", cfg.Mode)
	}
	if cfg.AdminKey != "" {
		t.Errorf("AdminKey = %q, want empty", cfg.AdminKey)
	}
	if cfg.OAuth.DCREnabled {
		t.Error("expected DCREnabled=false")
	}
}

func TestLoadAuthConfig_WithEnv(t *testing.T) {
	t.Setenv("VORTEX_AUTH_MODE", "apikey")
	t.Setenv("VORTEX_ADMIN_KEY", "admin-secret")
	t.Setenv("VORTEX_PUBLIC_KEY", "public-secret")
	t.Setenv("VORTEX_OAUTH_ISSUER", "https://example.com")
	t.Setenv("VORTEX_OAUTH_DCR_ENABLED", "true")
	t.Setenv("VORTEX_OAUTH_FALLBACK_APIKEY", "true")

	cfg := LoadAuthConfig()
	if cfg.Mode != "apikey" {
		t.Errorf("Mode = %q, want apikey", cfg.Mode)
	}
	if cfg.AdminKey != "admin-secret" {
		t.Errorf("AdminKey = %q, want admin-secret", cfg.AdminKey)
	}
	if cfg.PublicKey != "public-secret" {
		t.Errorf("PublicKey = %q, want public-secret", cfg.PublicKey)
	}
	if cfg.OAuth.Issuer != "https://example.com" {
		t.Errorf("Issuer = %q, want https://example.com", cfg.OAuth.Issuer)
	}
	if !cfg.OAuth.DCREnabled {
		t.Error("expected DCREnabled=true")
	}
	if !cfg.OAuth.FallbackAPIKey {
		t.Error("expected FallbackAPIKey=true")
	}
}

func TestGetenv(t *testing.T) {
	t.Setenv("TEST_GETENV_VAR", "value")
	if v := getenv("TEST_GETENV_VAR", "default"); v != "value" {
		t.Errorf("got %q, want value", v)
	}
	if v := getenv("TEST_GETENV_UNSET", "default"); v != "default" {
		t.Errorf("got %q, want default", v)
	}
	t.Setenv("TEST_GETENV_EMPTY", "")
	if v := getenv("TEST_GETENV_EMPTY", "default"); v != "default" {
		t.Errorf("got %q, want default for empty env", v)
	}
}

func TestMergeUnique(t *testing.T) {
	result := mergeUnique([]string{"a", "b"}, []string{"b", "c"})
	if len(result) != 3 {
		t.Fatalf("expected 3 items, got %d: %v", len(result), result)
	}
	expected := map[string]bool{"a": true, "b": true, "c": true}
	for _, s := range result {
		if !expected[s] {
			t.Errorf("unexpected item %q", s)
		}
	}
	result = mergeUnique(nil, []string{"x"})
	if len(result) != 1 || result[0] != "x" {
		t.Errorf("expected [x], got %v", result)
	}
}

func TestMergeMCPBindings(t *testing.T) {
	base := []MCPBinding{{MCPID: "mcp1"}, {MCPID: "mcp2"}}
	extra := []MCPBinding{{MCPID: "mcp2"}, {MCPID: "mcp3"}}
	result := mergeMCPBindings(base, extra)
	if len(result) != 3 {
		t.Fatalf("expected 3 bindings, got %d", len(result))
	}
	ids := map[string]bool{}
	for _, b := range result {
		ids[b.MCPID] = true
	}
	if !ids["mcp1"] || !ids["mcp2"] || !ids["mcp3"] {
		t.Errorf("expected mcp1, mcp2, mcp3, got %v", result)
	}
}

type patchTestTarget struct {
	A string `json:"a"`
	B string `json:"b"`
}

func TestApplyPatch_Move(t *testing.T) {
	target := &patchTestTarget{A: "value_a", B: ""}
	ops := []PatchOp{
		{Op: OpMove, From: "/a", Path: "/b"},
	}
	if err := ApplyPatch(target, ops); err != nil {
		t.Fatalf("ApplyPatch move failed: %v", err)
	}
	if target.A != "" {
		t.Errorf("expected a to be removed, got %q", target.A)
	}
	if target.B != "value_a" {
		t.Errorf("expected b to have moved value, got %q", target.B)
	}
}

func TestApplyPatch_Copy(t *testing.T) {
	target := &patchTestTarget{A: "value_a", B: ""}
	ops := []PatchOp{
		{Op: OpCopy, From: "/a", Path: "/b"},
	}
	if err := ApplyPatch(target, ops); err != nil {
		t.Fatalf("ApplyPatch copy failed: %v", err)
	}
	if target.A != "value_a" {
		t.Errorf("expected a to remain, got %q", target.A)
	}
	if target.B != "value_a" {
		t.Errorf("expected b to have copied value, got %q", target.B)
	}
}

func TestApplyPatch_Move_NotFound(t *testing.T) {
	target := map[string]any{"a": "value_a"}
	ops := []PatchOp{
		{Op: OpMove, From: "/nonexistent", Path: "/b"},
	}
	err := ApplyPatch(target, ops)
	if err == nil {
		t.Fatal("expected error for move from nonexistent path")
	}
}

func TestApplyPatch_Copy_NotFound(t *testing.T) {
	target := &patchTestTarget{A: "value_a"}
	ops := []PatchOp{
		{Op: OpCopy, From: "/nonexistent", Path: "/b"},
	}
	err := ApplyPatch(target, ops)
	if err == nil {
		t.Fatal("expected error for copy from nonexistent path")
	}
}

func TestGetHealthyModelsSummary(t *testing.T) {
	r := &Registry{
		Providers: map[string]*ProviderConfig{
			"p1": {Provider: "openai", Model: "gpt-4o", Capabilities: []string{"chat"}, Archetype: "reasoner"},
			"p2": {Provider: "anthropic", Model: "claude-3"},
		},
	}
	summary := r.GetHealthyModelsSummary()
	if len(summary) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(summary))
	}
	found := false
	for _, s := range summary {
		if s["id"] == "p1" {
			found = true
			if s["model"] != "gpt-4o" {
				t.Errorf("model = %v, want gpt-4o", s["model"])
			}
		}
	}
	if !found {
		t.Error("expected to find p1 in summary")
	}
}

func TestGetHealthyModelsSummary_FiltersExpired(t *testing.T) {
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &Registry{
		Providers: map[string]*ProviderConfig{
			"expired": {Provider: "x", Model: "m", ValidUntil: &past},
			"valid":   {Provider: "y", Model: "m2"},
		},
	}
	summary := r.GetHealthyModelsSummary()
	if len(summary) != 1 {
		t.Fatalf("expected 1 entry (valid only), got %d", len(summary))
	}
	if summary[0]["id"] != "valid" {
		t.Errorf("expected 'valid', got %v", summary[0]["id"])
	}
}

func TestResolveCapabilities_SkillsOnly(t *testing.T) {
	r := &Registry{
		Skills: map[string]*Skill{
			"s1": {ID: "s1", Capability: "coding"},
		},
	}
	skills, bindings := r.ResolveCapabilities([]string{"coding"})
	if len(skills) != 1 || skills[0] != "s1" {
		t.Errorf("skills = %v, want [s1]", skills)
	}
	if len(bindings) != 0 {
		t.Errorf("expected no bindings, got %v", bindings)
	}
}

func TestResolveCapabilities_MCPsOnly(t *testing.T) {
	r := &Registry{
		MCPs: map[string]*MCPDef{
			"mcp1": {ID: "mcp1", AvailableTools: []string{"search"}},
		},
	}
	skills, bindings := r.ResolveCapabilities([]string{"search"})
	if len(skills) != 0 {
		t.Errorf("expected no skills, got %v", skills)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}
	if bindings[0].MCPID != "mcp1" {
		t.Errorf("MCPID = %q, want mcp1", bindings[0].MCPID)
	}
}

func TestResolveCapabilities_Empty(t *testing.T) {
	r := &Registry{}
	skills, bindings := r.ResolveCapabilities(nil)
	if skills != nil || bindings != nil {
		t.Error("expected nil for empty caps")
	}
}

func TestResolveCapabilitiesToBindings_Empty(t *testing.T) {
	r := &Registry{}
	result := r.ResolveCapabilitiesToBindings(nil)
	if result != nil {
		t.Error("expected nil for empty caps")
	}
}

func TestResolveCapabilitiesToBindings_ProvidesMatch(t *testing.T) {
	r := &Registry{
		MCPs: map[string]*MCPDef{
			"mcp1": {ID: "mcp1", Provides: []string{"translation"}},
		},
	}
	result := r.ResolveCapabilitiesToBindings([]string{"translation"})
	if len(result) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(result))
	}
	if result[0].MCPID != "mcp1" {
		t.Errorf("MCPID = %q, want mcp1", result[0].MCPID)
	}
}

func TestResolveCapabilitiesToBindings_PrefixMatch(t *testing.T) {
	r := &Registry{
		MCPs: map[string]*MCPDef{
			"mcp1": {ID: "mcp1", AvailableTools: []string{"search"}},
		},
	}
	result := r.ResolveCapabilitiesToBindings([]string{"mcp1.search"})
	if len(result) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(result))
	}
	if result[0].MCPID != "mcp1" {
		t.Errorf("MCPID = %q, want mcp1", result[0].MCPID)
	}
}

func TestCheckCapabilities_AllFound(t *testing.T) {
	r := &Registry{
		Skills: map[string]*Skill{
			"s1": {ID: "s1", Capability: "coding"},
		},
		MCPs: map[string]*MCPDef{
			"mcp1": {ID: "mcp1", AvailableTools: []string{"search"}},
		},
	}
	missing := r.CheckCapabilities([]string{"coding", "search"})
	if len(missing) != 0 {
		t.Errorf("expected no missing, got %v", missing)
	}
}

func TestCheckCapabilities_SomeMissing(t *testing.T) {
	r := &Registry{
		Skills: map[string]*Skill{
			"s1": {ID: "s1", Capability: "coding"},
		},
	}
	missing := r.CheckCapabilities([]string{"coding", "nonexistent"})
	if len(missing) != 1 {
		t.Fatalf("expected 1 missing, got %d", len(missing))
	}
	if missing[0] != "nonexistent" {
		t.Errorf("missing = %v, want [nonexistent]", missing)
	}
}

func TestCheckCapabilities_Empty(t *testing.T) {
	r := &Registry{}
	if missing := r.CheckCapabilities(nil); missing != nil {
		t.Error("expected nil for empty caps")
	}
}

func TestOnReload(t *testing.T) {
	r := &Registry{}
	called := false
	r.OnReload(func() { called = true })
	if len(r.reloadCallbacks) != 1 {
		t.Fatalf("expected 1 callback, got %d", len(r.reloadCallbacks))
	}
	r.reloadCallbacks[0]()
	if !called {
		t.Error("expected callback to be called")
	}
}

func TestSetEnvReloadHook(t *testing.T) {
	r := &Registry{}
	called := false
	r.SetEnvReloadHook(func() { called = true })
	if r.envReloadHook == nil {
		t.Fatal("expected hook to be set")
	}
	r.envReloadHook()
	if !called {
		t.Error("expected hook to be called")
	}
}

func TestLoadRaw(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	content := []byte(`{"providers": {}}`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	r := &Registry{configPath: path}
	data, err := r.LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw failed: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("data = %q, want %q", data, content)
	}
}

func TestLoadRaw_NotFound(t *testing.T) {
	r := &Registry{configPath: "/nonexistent/path.json"}
	_, err := r.LoadRaw()
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestStopWatcher_NoOp(t *testing.T) {
	r := &Registry{}
	r.StopWatcher()
}

func TestCallableByAdmin(t *testing.T) {
	tests := []struct {
		name     string
		role     *Role
		isAdmin  bool
		want     bool
	}{
		{"nil role", nil, true, true},
		{"empty tiers", &Role{ID: "r1"}, true, true},
		{"admin tier admin caller", &Role{ID: "r1", CallableTiers: []string{"admin"}}, true, true},
		{"admin tier public caller", &Role{ID: "r1", CallableTiers: []string{"admin"}}, false, false},
		{"public tier public caller", &Role{ID: "r1", CallableTiers: []string{"public"}}, false, true},
		{"public tier admin caller", &Role{ID: "r1", CallableTiers: []string{"public"}}, true, false},
		{"wildcard", &Role{ID: "r1", CallableTiers: []string{"*"}}, false, true},
		{"both tiers admin", &Role{ID: "r1", CallableTiers: []string{"admin", "public"}}, true, true},
		{"both tiers public", &Role{ID: "r1", CallableTiers: []string{"admin", "public"}}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.role.CallableByAdmin(tt.isAdmin); got != tt.want {
				t.Errorf("CallableByAdmin(%v) = %v, want %v", tt.isAdmin, got, tt.want)
			}
		})
	}
}

func TestToolBindingWarning_StringCategories(t *testing.T) {
	w := ToolBindingWarning{RoleID: "r1", MCPID: "m1", Category: "binding_mcp_missing"}
	if w.String() == "" {
		t.Error("expected non-empty string for binding_mcp_missing")
	}
	w = ToolBindingWarning{RoleID: "r1", MCPID: "m1", Tool: "t1", Category: "binding_tool_missing"}
	if w.String() == "" {
		t.Error("expected non-empty string for binding_tool_missing")
	}
	w = ToolBindingWarning{RoleID: "r1", Tool: "t1", Category: "instruction_ghost"}
	if w.String() == "" {
		t.Error("expected non-empty string for instruction_ghost")
	}
	w = ToolBindingWarning{RoleID: "r1", Detail: "custom", Category: "unknown"}
	if w.String() == "" {
		t.Error("expected non-empty string for unknown category")
	}
}

func TestCompactor_StartCancel(t *testing.T) {
	r := &Registry{}
	c := NewCompactor(r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Start(ctx)
}

func TestCompactor_CheckAndCompact_NoWAL(t *testing.T) {
	r := &Registry{}
	c := NewCompactor(r)
	c.checkAndCompact()
}
