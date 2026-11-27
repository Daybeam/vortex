package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Register O47 regression: require_audit used to have no producer and resolved
// verifier names never reached the audit spawn request.
func TestRequireAudit_VerifierResolutionPriority(t *testing.T) {
	tests := []struct {
		name     string
		step     schemas.Step
		system   config.SystemSettings
		provider string
		model    string
		fallback bool
	}{
		{name: "explicit", step: schemas.Step{VerifierModel: "explicit-model"}, system: config.SystemSettings{VerifierProvider: "system", VerifierModel: "system-model"}, model: "explicit-model"},
		{name: "system pair", step: schemas.Step{RequireAudit: true}, system: config.SystemSettings{VerifierProvider: "system", VerifierModel: "system-model"}, provider: "system", model: "system-model"},
		{name: "system provider only", step: schemas.Step{RequireAudit: true}, system: config.SystemSettings{VerifierProvider: "system"}, provider: "system"},
		{name: "system model only", step: schemas.Step{RequireAudit: true}, system: config.SystemSettings{VerifierModel: "system-model"}, model: "system-model"},
		{name: "default", step: schemas.Step{RequireAudit: true}, provider: "main", fallback: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &config.Registry{DefaultProvider: "main", System: tt.system}
			logger := mustNewLogger(t, mustTempDir(t, "verifier-log"), &reg.System)
			engine := &DirectedEngine{registry: reg, logger: logger}
			graph := &schemas.TaskGraph{TaskID: "task", Steps: map[string]*schemas.Step{"s1": &tt.step}}
			engine.resolveGraphVerifiers(graph)
			got := graph.Steps["s1"]
			if got.VerifierProvider != tt.provider || got.VerifierModel != tt.model {
				t.Fatalf("resolved %s/%s, want %s/%s", got.VerifierProvider, got.VerifierModel, tt.provider, tt.model)
			}
			subs := graph.GetSubstitutions()
			if (len(subs) == 1) != tt.fallback {
				t.Fatalf("substitution ledger = %#v, fallback=%v", subs, tt.fallback)
			}
		})
	}
}

func TestModelOverride_SelectsProviderModel(t *testing.T) {
	reg := &config.Registry{Providers: map[string]*config.ProviderConfig{
		"main": {Provider: "openai", Model: "main-model"},
	}}
	spawner := &Spawner{}
	pc := spawner.resolveCandidateConfig(NewContextHub(reg, nil, nil), nil, "main", "audit-model")
	if pc == nil || pc.Model != "audit-model" {
		t.Fatalf("resolved config = %#v, want audit-model", pc)
	}
}

func TestInheritCallerSettings_PreservesPlannerAuditIntent(t *testing.T) {
	planned := []schemas.StepInput{{ID: "s2", DependsOn: []string{"s1"}, RequireAudit: true}}
	inheritCallerSettings(planned, schemas.StepInput{})
	if !planned[0].RequireAudit {
		t.Fatal("caller inheritance erased planner-declared require_audit")
	}
}

// Register O47 regression: resolving before surgery injection left replacement
// audit steps without deployment verifier policy.
func TestRequireAudit_SurgeryResolvesReplacementStep(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "main",
		System:          config.SystemSettings{VerifierProvider: "system", VerifierModel: "audit-model"},
	}
	logger := mustNewLogger(t, mustTempDir(t, "surgery-verifier-log"), &reg.System)
	engine := &DirectedEngine{registry: reg, logger: logger}
	graph := &schemas.TaskGraph{
		TaskID: "task",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "engineer", Status: schemas.StepFailed, LastError: "failed"},
		},
	}
	engine.graphs = map[string]*schemas.TaskGraph{"task": graph}
	err := engine.MutateGraphTopology("task", SurgeryData{
		DeprecatedStepID: "s1",
		ReplacementSteps: []schemas.StepInput{{ID: "s2", RoleID: "engineer", Task: "retry", RequireAudit: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := graph.Steps["s2"]
	if got.VerifierProvider != "system" || got.VerifierModel != "audit-model" {
		t.Fatalf("replacement resolved %s/%s, want system/audit-model", got.VerifierProvider, got.VerifierModel)
	}
}

// Register O47 regression: require_audit must reach semantic verification even
// when a regex criterion fails and the provider default leaves VerifierModel empty.
func TestRequireAudit_RegexMismatchStillRunsSemanticAudit(t *testing.T) {
	providers.ClearCache()
	t.Cleanup(providers.ClearCache)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"PASS"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, "data: [DONE]")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	reg := &config.Registry{
		DefaultProvider: "main",
		Providers: map[string]*config.ProviderConfig{
			"main": {Provider: "openai", Model: "main-model", BaseURL: srv.URL, APIKey: "fake"},
		},
		Roles: map[string]*config.Role{
			"worker":  {ID: "worker", BaseCapability: "text"},
			"auditor": {ID: "auditor", BaseCapability: "text"},
		},
	}
	tmp := t.TempDir()
	logger := mustNewLogger(t, mustTempDir(t, "audit-regex-log"), &reg.System)
	t.Cleanup(func() { logger.Close() })
	ts := store.NewTaskStore(store.NewFileTaskBackend(tmp))
	engine := NewDirectedEngine(reg, ts, nil, nil, logger, nil, tmp, tmp, nil)
	defer engine.Stop()
	graph := &schemas.TaskGraph{TaskID: "audit-task", Steps: map[string]*schemas.Step{}, DecisionHistory: map[string]*schemas.DecisionNode{}}
	step := &schemas.Step{
		ID: "s1", RoleID: "worker", Task: "produce output", ExitCriteria: "regex:NEVER_MATCH",
		RequireAudit: true, DynamicRubrics: []string{"verify the task"},
	}
	ok, reason, _ := engine.verifyExitCriteria(context.Background(), graph, step, &SpawnResult{
		Output: schemas.SubagentOutput{Result: map[string]any{"content": "actual output"}},
	})
	if !ok || reason != "" {
		t.Fatalf("semantic audit result = %v/%q, want true/empty", ok, reason)
	}
	if calls.Load() == 0 {
		t.Fatal("semantic audit was not called")
	}
}

// Register O47 regression: the planner prompt must not forbid the audit-intent
// field that SOP/planner conversion now supports.
func TestPlannerPrompt_AllowsAuditIntent(t *testing.T) {
	raw, err := os.ReadFile("scheduler_planner.go")
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(raw)
	if strings.Contains(prompt, "Do NOT emit a verification/audit step") {
		t.Fatal("planner prompt still forbids explicit verifier steps")
	}
	if !strings.Contains(prompt, "require_audit") || !strings.Contains(prompt, "explicit verifier step") {
		t.Fatal("planner prompt does not explain require_audit and explicit verifier steps")
	}
}
