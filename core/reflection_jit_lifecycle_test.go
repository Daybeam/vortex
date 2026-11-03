package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/jit"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func newMockJITRegistry(t *testing.T, response string) (*config.Registry, *MockHybridProvider) {
	t.Helper()
	// providers.Get caches by ProviderConfig key for the whole process, so a
	// fixed "mock-hybrid" name would hand later tests the first test's mock.
	name := fmt.Sprintf("mock-jit-%d", time.Now().UnixNano())
	cfg := &config.ProviderConfig{Provider: name}
	reg := &config.Registry{
		Providers:       map[string]*config.ProviderConfig{"default": cfg},
		DefaultProvider: name,
		Roles:           make(map[string]*config.Role),
		Skills:          make(map[string]*config.Skill),
		MCPs:            make(map[string]*config.MCPDef),
		DynamicMCPs:     make(map[string]*config.MCPDef),
	}
	// DefaultProvider must resolve through reg.Providers, so point the alias
	// back at the unique name as well.
	reg.Providers[name] = cfg
	mock := &MockHybridProvider{response: response}
	providers.GlobalRouter.Register(name, cfg, mock)
	t.Cleanup(func() { providers.GlobalRouter.Deregister(name) })
	return reg, mock
}

// TestCrystallizeStepToSkill_CapturesGeneratedSource: the GeneratedSkill record
// used to carry only a description, so once the JIT tool's TTL expired the
// generated script body was unrecoverable. The body must be persisted on the
// record at crystallization time.
func TestCrystallizeStepToSkill_CapturesGeneratedSource(t *testing.T) {
	tmp := t.TempDir()
	reg, _ := newMockJITRegistry(t, "```lua\n-- captured body\n```")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jitMgr, logger)

	re.CrystallizeStepToSkill(context.Background(), store.StepRecord{
		StepID: "s1", RoleID: "r1", Capability: "capture-test", Status: "OK",
		Task:  "generate a reusable tool",
		Trace: []store.ToolInteraction{{ToolName: "shell"}},
	})

	var found *store.GeneratedSkill
	for _, gs := range es.GetGeneratedSkillsSnapshot() {
		if gs.Description == "generate a reusable tool" {
			found = gs
		}
	}
	if found == nil {
		t.Fatal("crystallized skill record not found")
	}
	if found.SourceText != "-- captured body" {
		t.Fatalf("GeneratedSkill.SourceText must hold the generated body, got %q", found.SourceText)
	}
	if src, err := jitMgr.GetSource(found.ID); err != nil || src != found.SourceText {
		t.Fatalf("persisted body must match the live tool source (src=%q, err=%v)", src, err)
	}
}

// TestCrystallizeStepToSkill_UsesStoredSourceWhenToolExpired reproduces the
// additive-editing baseline loss: JIT tools expire after 24h, so GetSource
// fails and the prompt used to omit the existing implementation entirely. The
// body persisted on the skill record must seed the additive edit instead.
// Regression for JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §7.6.
func TestCrystallizeStepToSkill_UsesStoredSourceWhenToolExpired(t *testing.T) {
	tmp := t.TempDir()
	reg, mock := newMockJITRegistry(t, "```lua\n-- additive extension\n```")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jitMgr, logger)

	const baseline = "-- persisted baseline"
	if err := es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
		ID: "jit_expired", Capability: "addon-test", SuccessRate: 1.0,
		IsVerified: true, SourceText: baseline,
	}); err != nil {
		t.Fatalf("AddGeneratedSkill: %v", err)
	}
	if reg.GetMCP("jit_expired") != nil {
		t.Fatal("test setup: jit_expired must not be a registered tool")
	}

	re.CrystallizeStepToSkill(context.Background(), store.StepRecord{
		StepID: "s1", RoleID: "r1", Capability: "addon-test", Status: "OK",
		Task:  "new task variant",
		Trace: []store.ToolInteraction{{ToolName: "shell"}},
	})

	if !strings.Contains(mock.lastSystem, baseline) {
		t.Fatalf("expired tool's stored body must seed additive editing, system prompt was %q", mock.lastSystem)
	}
}

// TestScanForHybridMerge_UsesStoredSourceWhenToolsExpired is the merge half of
// the same defect: expired variants left `sources` empty, so the merge was
// skipped forever even though every skill record still knew its body.
// Regression for JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §7.6.
func TestScanForHybridMerge_UsesStoredSourceWhenToolsExpired(t *testing.T) {
	tmp := t.TempDir()
	reg, mock := newMockJITRegistry(t, "```lua\n-- unified expert\n```")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	re := NewReflectionEngine(es, nil, reg, jitMgr, logger)

	for id, src := range map[string]string{
		"jit_v1": "-- variant one",
		"jit_v2": "-- variant two",
	} {
		if err := es.AddGeneratedSkill(context.Background(), &store.GeneratedSkill{
			ID: id, Capability: "merge-test", SuccessRate: 1.0, IsVerified: true,
			UsageCount: 5, SourceText: src,
		}); err != nil {
			t.Fatalf("AddGeneratedSkill(%s): %v", id, err)
		}
	}

	re.ScanForHybridMerge(context.Background())

	if !strings.Contains(mock.lastUser, "-- variant one") || !strings.Contains(mock.lastUser, "-- variant two") {
		t.Fatalf("expired variants' stored bodies must reach the merge prompt, got %q", mock.lastUser)
	}
	var merged *store.GeneratedSkill
	for _, gs := range es.GetGeneratedSkillsSnapshot() {
		if gs.Description == "Unified Expert Skill via Hybrid Merge" {
			merged = gs
		}
	}
	if merged == nil {
		t.Fatal("hybrid merge did not register a unified skill")
	}
	if merged.SourceText != "-- unified expert" {
		t.Fatalf("merged skill must carry its generated body, got %q", merged.SourceText)
	}
}

// TestReflectOnTask_CapturesTraceJITSource covers the second seeding path:
// tools discovered in a successful trace were recorded with no description and
// no body, so their records could never serve as an editing baseline.
func TestReflectOnTask_CapturesTraceJITSource(t *testing.T) {
	tmp := t.TempDir()
	reg, _ := newMockJITRegistry(t, "-- unused")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)
	ts := &mockSignalTaskStore{}
	re := NewReflectionEngine(es, ts, reg, jitMgr, logger)

	toolID, err := jitMgr.RegisterTool("-- trace tool body", "lua", time.Hour, true)
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	reg.Roles["r1"] = &config.Role{ID: "r1", BaseCapability: "analyze"}

	conf := 0.9
	graph := &schemas.TaskGraph{
		TaskID: "task_trace_source", Status: schemas.GraphCompleted,
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "r1", Task: "use the generated tool", Status: schemas.StepOK, Confidence: &conf},
		},
	}
	if err := ts.Set(context.Background(), graph.TaskID, "s1", &store.StepResult{
		Trace: []store.ToolInteraction{{ToolName: toolID}},
	}); err != nil {
		t.Fatalf("TaskStore.Set: %v", err)
	}

	re.ReflectOnTask(context.Background(), graph)
	re.Wait()
	es.WaitAsyncSaves()

	gs, ok := es.GetGeneratedSkill(toolID)
	if !ok {
		t.Fatalf("trace-seeded skill %q not found", toolID)
	}
	if gs.SourceText != "-- trace tool body" {
		t.Fatalf("trace seed must capture the tool body, got %q", gs.SourceText)
	}
}

// TestNewSpawner_FiltersExpiredJITToolsFromRetrieval verifies production
// wiring: a spawner must install the registry-backed liveness probe so
// spawn-path retrieval never advertises a tool that expired or was never
// registered in this process.
func TestNewSpawner_FiltersExpiredJITToolsFromRetrieval(t *testing.T) {
	tmp := t.TempDir()
	reg, _ := newMockJITRegistry(t, "-- unused")
	logger := mustNewLogger(t, t.TempDir(), nil)
	jitMgr := jit.NewJITManager(reg, tmp)
	es := mustNewExperienceStore(t, tmp, nil, nil, nil, nil)

	liveID, err := jitMgr.RegisterTool("-- live tool", "lua", time.Hour, true)
	if err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	desc := "audit authentication flow for security issues"
	es.GeneratedSkills[liveID] = &store.GeneratedSkill{
		ID: liveID, Description: desc, Capability: "security", SuccessRate: 1.0,
	}
	es.GeneratedSkills["jit_expired"] = &store.GeneratedSkill{
		ID: "jit_expired", Description: desc, Capability: "security", SuccessRate: 1.0,
	}

	NewSpawner(reg, nil, es, logger, nil, tmp)

	got := es.QueryRelevantSkills(context.Background(), desc, 2)
	if len(got) != 1 || got[0] != liveID {
		t.Fatalf("retrieval must return only the live JIT tool, got %v", got)
	}
}
