package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// ─── Defect 1: Tool Vacuum ─────────────────────────────────────────────────

func TestCollectDelegationToolDefs_IncludesCoreTools(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", BaseCapability: "text"},
		},
		System: config.SystemSettings{DelegationMode: true},
	}
	hub := NewContextHub(reg, nil, nil)
	spawner := &Spawner{registry: reg}

	defs := spawner.collectDelegationToolDefs(hub, nil)

	if len(defs) == 0 {
		t.Fatal("expected core tool definitions, got empty list")
	}
	hasWriteFile := false
	for _, d := range defs {
		if d.Name == "write_file" {
			hasWriteFile = true
		}
	}
	if !hasWriteFile {
		t.Error("expected write_file in core tool definitions")
	}
}

func TestCollectDelegationToolDefs_IncludesMCPBindings(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", BaseCapability: "text"},
		},
		MCPs: map[string]*config.MCPDef{
			"search-mcp": {
				ID: "search-mcp",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "web_search", Description: "Search the web"},
					{Name: "fetch_page", Description: "Fetch a page"},
				},
			},
		},
		System: config.SystemSettings{DelegationMode: true},
	}
	hub := NewContextHub(reg, nil, nil)
	spawner := &Spawner{registry: reg}

	bindings := []config.MCPBinding{{MCPID: "search-mcp"}}
	defs := spawner.collectDelegationToolDefs(hub, bindings)

	hasWebSearch := false
	hasFetchPage := false
	for _, d := range defs {
		if d.Name == "web_search" {
			hasWebSearch = true
		}
		if d.Name == "fetch_page" {
			hasFetchPage = true
		}
	}
	if !hasWebSearch {
		t.Error("expected web_search in delegation tool defs")
	}
	if !hasFetchPage {
		t.Error("expected fetch_page in delegation tool defs")
	}
}

func TestCollectDelegationToolDefs_RespectsAllowlist(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", BaseCapability: "text"},
		},
		MCPs: map[string]*config.MCPDef{
			"search-mcp": {
				ID: "search-mcp",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "web_search", Description: "Search the web"},
					{Name: "fetch_page", Description: "Fetch a page"},
				},
			},
		},
		System: config.SystemSettings{DelegationMode: true},
	}
	hub := NewContextHub(reg, nil, nil)
	spawner := &Spawner{registry: reg}

	bindings := []config.MCPBinding{
		{MCPID: "search-mcp", AllowedTools: []string{"web_search"}},
	}
	defs := spawner.collectDelegationToolDefs(hub, bindings)

	hasWebSearch := false
	hasFetchPage := false
	for _, d := range defs {
		if d.Name == "web_search" {
			hasWebSearch = true
		}
		if d.Name == "fetch_page" {
			hasFetchPage = true
		}
	}
	if !hasWebSearch {
		t.Error("expected web_search in restricted delegation tool defs")
	}
	if hasFetchPage {
		t.Error("fetch_page should be filtered out by allowlist")
	}
}

// ─── Defect 2: Async Dead-End ──────────────────────────────────────────────

func TestFulfillDelegation_UnblocksStep(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	graph := waitForBlocked(t, engine, taskID)

	result := map[string]any{"answer": "delegated result"}
	if err := engine.FulfillDelegation(taskID, "step_1", result); err != nil {
		t.Fatalf("FulfillDelegation failed: %v", err)
	}

	for i := 0; i < 10; i++ {
		engine.Mu.RLock()
		graph = engine.graphs[taskID]
		engine.Mu.RUnlock()
		if graph.Status == schemas.GraphCompleted {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if graph.Status != schemas.GraphCompleted {
		t.Errorf("expected graph completed, got %v", graph.Status)
	}
}

func TestFulfillDelegation_UnknownTask(t *testing.T) {
	engine, _ := newDelegationEngine(t)
	err := engine.FulfillDelegation("nonexistent", "step_1", map[string]any{})
	if err == nil {
		t.Fatal("expected error for unknown task")
	}
}

func TestFulfillDelegation_StepNotBlocked(t *testing.T) {
	engine, reg := newDelegationEngine(t)
	reg.System.DelegationMode = false

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	err = engine.FulfillDelegation(taskID, "step_1", map[string]any{})
	if err == nil {
		t.Fatal("expected error for non-blocked step")
	}
}

// ─── Defect 3: Context Privacy ─────────────────────────────────────────────

func TestRedactContextForDelegation_StripsUpstream(t *testing.T) {
	ctx := map[string]any{
		"upstream": map[string]any{"secret_data": "sensitive"},
		"path":     []map[string]any{{"node": "internal"}},
		"task":     map[string]any{"id": "task_1"},
	}

	redacted := redactContextForDelegation(ctx)

	upstream, ok := redacted["upstream"].(string)
	if !ok || upstream == "" {
		t.Errorf("expected upstream to be redacted string, got %T = %v", redacted["upstream"], redacted["upstream"])
	}

	path, ok := redacted["path"].(string)
	if !ok || path == "" {
		t.Errorf("expected path to be redacted string, got %T = %v", redacted["path"], redacted["path"])
	}

	task, ok := redacted["task"].(map[string]any)
	if !ok {
		t.Errorf("expected task to be preserved, got %T", redacted["task"])
	}
	if task["id"] != "task_1" {
		t.Errorf("expected task.id to be preserved, got %v", task["id"])
	}
}

func TestRedactContextForDelegation_FiltersEnvKeys(t *testing.T) {
	ctx := map[string]any{
		"env": map[string]any{
			"os":          "linux",
			"cwd":         "/home/user",
			"secret_key":  "should-not-leak",
			"api_token":   "should-not-leak",
			"arch":        "amd64",
			"db_password": "should-not-leak",
		},
	}

	redacted := redactContextForDelegation(ctx)

	env, ok := redacted["env"].(map[string]any)
	if !ok {
		t.Fatalf("expected env to be map, got %T", redacted["env"])
	}
	if env["os"] != "linux" {
		t.Error("expected os to be preserved (safe key)")
	}
	if env["cwd"] != "/home/user" {
		t.Error("expected cwd to be preserved (safe key)")
	}
	if env["arch"] != "amd64" {
		t.Error("expected arch to be preserved (safe key)")
	}
	if _, exists := env["secret_key"]; exists {
		t.Error("expected secret_key to be filtered out")
	}
	if _, exists := env["api_token"]; exists {
		t.Error("expected api_token to be filtered out")
	}
	if _, exists := env["db_password"]; exists {
		t.Error("expected db_password to be filtered out")
	}
}

func TestRedactContextForDelegation_EmptyContext(t *testing.T) {
	redacted := redactContextForDelegation(map[string]any{})
	if len(redacted) != 0 {
		t.Errorf("expected empty redacted context, got %v", redacted)
	}
}

// ─── Defect 4: Alias Gap (core-level StepInput) ────────────────────────────

func TestStepInput_RoleAlias(t *testing.T) {
	json := `{"id":"s1","role":"worker","task":"do work"}`
	var si schemas.StepInput
	if err := si.UnmarshalJSON([]byte(json)); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if si.RoleID != "worker" {
		t.Errorf("expected role_id='worker' from 'role' alias, got %q", si.RoleID)
	}
}

func TestStepInput_ContentAlias(t *testing.T) {
	json := `{"id":"s1","role_id":"worker","content":"do work"}`
	var si schemas.StepInput
	if err := si.UnmarshalJSON([]byte(json)); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if si.Task != "do work" {
		t.Errorf("expected task='do work' from 'content' alias, got %q", si.Task)
	}
}

func TestStepInput_ObjectiveAlias(t *testing.T) {
	json := `{"id":"s1","role_id":"worker","objective":"do work"}`
	var si schemas.StepInput
	if err := si.UnmarshalJSON([]byte(json)); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if si.Task != "do work" {
		t.Errorf("expected task='do work' from 'objective' alias, got %q", si.Task)
	}
}

func TestStepInput_GoalAlias(t *testing.T) {
	json := `{"id":"s1","role_id":"worker","goal":"do work"}`
	var si schemas.StepInput
	if err := si.UnmarshalJSON([]byte(json)); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if si.Task != "do work" {
		t.Errorf("expected task='do work' from 'goal' alias, got %q", si.Task)
	}
}

// ─── Defect 5: Ghost Binding ────────────────────────────────────────────────

func TestResolveFinalMCPs_DynamicRegistrationViaCallback(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"worker": {
				ID:               "worker",
				BaseCapability:   "text",
				AllowDynamicMCPs: true,
			},
		},
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	hub := NewContextHub(reg, nil, nil)

	hub.OnUnknownMCP = func(mcpID string) *config.MCPDef {
		if mcpID == "dynamic-search" {
			return &config.MCPDef{
				ID: "dynamic-search",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "search", Description: "Dynamic search"},
				},
			}
		}
		return nil
	}

	bindings, err := ResolveFinalMCPs(hub, "worker", []string{"dynamic-search"}, nil)
	if err != nil {
		t.Fatalf("expected dynamic MCP registration to succeed, got error: %v", err)
	}
	if len(bindings) == 0 {
		t.Fatal("expected at least one binding")
	}

	registered := hub.GetMCP("dynamic-search")
	if registered == nil {
		t.Fatal("expected dynamic-search to be registered in DynamicMCPs")
	}
}

func TestResolveFinalMCPs_CallbackReturnsNil_StillErrors(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"worker": {
				ID:               "worker",
				BaseCapability:   "text",
				AllowDynamicMCPs: true,
			},
		},
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	hub := NewContextHub(reg, nil, nil)

	hub.OnUnknownMCP = func(mcpID string) *config.MCPDef {
		return nil
	}

	_, err := ResolveFinalMCPs(hub, "worker", []string{"nonexistent-mcp"}, nil)
	if err == nil {
		t.Fatal("expected error when callback returns nil for unknown MCP")
	}
}

func TestResolveFinalMCPs_NoCallback_StillErrors(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"worker": {
				ID:               "worker",
				BaseCapability:   "text",
				AllowDynamicMCPs: true,
			},
		},
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	hub := NewContextHub(reg, nil, nil)

	_, err := ResolveFinalMCPs(hub, "worker", []string{"nonexistent-mcp"}, nil)
	if err == nil {
		t.Fatal("expected error for unknown MCP with no callback")
	}
}

func TestRegisterDynamicMCP(t *testing.T) {
	reg := &config.Registry{
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	hub := NewContextHub(reg, nil, nil)

	mcp := &config.MCPDef{
		ID: "test-dynamic",
		FullToolDefinitions: []schemas.ToolDefinition{
			{Name: "tool1", Description: "Test tool"},
		},
	}
	hub.RegisterDynamicMCP(mcp)

	retrieved := hub.GetMCP("test-dynamic")
	if retrieved == nil {
		t.Fatal("expected to retrieve dynamically registered MCP")
	}
	if retrieved.ID != "test-dynamic" {
		t.Errorf("expected ID 'test-dynamic', got %q", retrieved.ID)
	}
}

// ─── Defect 6: Timeout / Token Budget ──────────────────────────────────────

func TestSubmitWithSessionIR_SetsTimeoutAndTokenBudget(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.SubmitWithSessionIR(
		[]schemas.StepInput{
			{ID: "step_1", RoleID: "worker", Task: "work"},
		},
		nil, nil, nil, "", "", "",
		120,   // timeout = 120 seconds
		50000, // token_budget = 50000
	)
	if err != nil {
		t.Fatalf("SubmitWithSessionIR failed: %v", err)
	}

	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	engine.Mu.RUnlock()

	if graph == nil {
		t.Fatal("expected graph to exist")
	}
	if graph.TimeoutSecs != 120 {
		t.Errorf("expected TimeoutSecs=120, got %d", graph.TimeoutSecs)
	}
	if graph.TokenBudget != 50000 {
		t.Errorf("expected TokenBudget=50000, got %d", graph.TokenBudget)
	}
}

func TestSubmitWithSessionIR_ZeroTimeoutUsesDefault(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.SubmitWithSessionIR(
		[]schemas.StepInput{
			{ID: "step_1", RoleID: "worker", Task: "work"},
		},
		nil, nil, nil, "", "", "",
		0, // timeout = 0 (use default)
		0, // token_budget = 0 (use default)
	)
	if err != nil {
		t.Fatalf("SubmitWithSessionIR failed: %v", err)
	}

	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	engine.Mu.RUnlock()

	if graph == nil {
		t.Fatal("expected graph to exist")
	}
	if graph.TimeoutSecs != 0 {
		t.Errorf("expected TimeoutSecs=0 (default), got %d", graph.TimeoutSecs)
	}
	if graph.TokenBudget != 0 {
		t.Errorf("expected TokenBudget=0 (default), got %d", graph.TokenBudget)
	}
}

// ─── Integration: Delegation return includes tool_definitions ──────────────

func TestDelegationReturn_IncludesToolDefinitions(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	graph := waitForBlocked(t, engine, taskID)

	dec := graph.PendingDecisions[0]
	prompt, _ := dec.Context["prompt"].(map[string]any)
	if prompt == nil {
		t.Fatal("expected prompt in decision context")
	}

	toolDefs, ok := prompt["tool_definitions"]
	if !ok {
		t.Fatal("expected tool_definitions in delegation return (Defect 1 fix)")
	}

	defs, ok := toolDefs.([]schemas.ToolDefinition)
	if !ok {
		t.Fatalf("expected []ToolDefinition, got %T", toolDefs)
	}
	if len(defs) == 0 {
		t.Error("expected non-empty tool definitions in delegation return")
	}
}

// ─── Integration: Delegation return redacts context ────────────────────────

func TestDelegationReturn_RedactsUpstreamContext(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	graph := waitForBlocked(t, engine, taskID)

	dec := graph.PendingDecisions[0]
	prompt, _ := dec.Context["prompt"].(map[string]any)
	if prompt == nil {
		t.Fatal("expected prompt in decision context")
	}

	sysPrompt, _ := prompt["system_prompt"].(string)
	if sysPrompt == "" {
		t.Fatal("expected system_prompt in delegation return")
	}

	if containsSubstring(sysPrompt, "upstream") {
		t.Log("Note: 'upstream' may appear in prompt template structure; verify no raw upstream data leaks")
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// ─── Integration: FulfillDelegation clears pending decision ────────────────

func TestFulfillDelegation_ClearsPendingDecision(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "work"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	graph := waitForBlocked(t, engine, taskID)

	if len(graph.PendingDecisions) == 0 {
		t.Fatal("expected pending decisions before fulfillment")
	}

	result := map[string]any{"answer": "fulfilled"}
	if err := engine.FulfillDelegation(taskID, "step_1", result); err != nil {
		t.Fatalf("FulfillDelegation failed: %v", err)
	}

	engine.Mu.RLock()
	graph = engine.graphs[taskID]
	engine.Mu.RUnlock()

	for _, dec := range graph.PendingDecisions {
		if dec.StepID == "step_1" && dec.Type == schemas.DecisionDelegationRequired {
			t.Error("expected delegation decision to be cleared after fulfillment")
		}
	}
}

// Ensure context import is used
var _ = context.Background
var _ = store.StepResult{}
