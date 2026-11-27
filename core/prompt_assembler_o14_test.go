package core

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// TestPromptAssembler_O14BlockOrder pins the complete sequential assembly
// contract across the O14 phase extraction. It intentionally enables every
// optional block so a phase moved before/after the constraints cache marker,
// or a volatile block reordered, fails at the interaction boundary.
func TestPromptAssembler_O14BlockOrder(t *testing.T) {
	tmpDir := t.TempDir()
	ts := store.NewTaskStore(store.NewFileTaskBackend(tmpDir))
	reg := &config.Registry{Roles: make(map[string]*config.Role)}
	reg.System.MemoryBankEnabled = boolPtr(true)
	logger := mustNewLogger(t, t.TempDir(), &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() })
	es := mustNewExperienceStore(t, tmpDir, ts, &reg.System, nil, nil)

	es.AntiPatternStore.Upsert(context.Background(), store.AntiPatternPrecedent{
		ID:             "o14-coding-pitfall",
		AntiPattern:    "coding without checking the existing implementation",
		CorrectPattern: "inspect the current code before editing",
		Symptom:        "duplicated logic",
		Confidence:     0.9,
	})
	// One coding pattern among unrelated corpus entries clears QuerySimilarPatterns'
	// BM25 relevance threshold; three identical coding patterns would not.
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		taskType, source := "analysis", "unrelated analysis task "+id
		if i == 0 {
			taskType, source = "coding", "coding task "+id
		}
		es.TaskPatterns[id] = store.TaskPattern{
			ID: id, TaskType: taskType, SourceText: source,
		}
	}
	es.Nodes["o14-failure"] = &store.ExperienceNode{
		NodeID:      "o14-failure",
		Capability:  "coding",
		ModelID:     "o14-model",
		Outcome:     "failure",
		FailureMode: "timeout_exceeded",
		Strategy:    "unbounded command",
		Critique:    "use an explicit timeout",
	}
	es.RebuildNodeIndex()

	mb := store.NewMemoryBankStore(filepath.Join(tmpDir, "memory"), nil)
	if err := mb.SaveUserProfile(store.UserProfile{Expertise: []string{"Go"}}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	archive := NewContextArchive(filepath.Join(tmpDir, "archive.jsonl"))
	mustAppend(t, archive, MemoryItem{
		TaskID: "o14-task", NodeID: "step-1", Summary: "prior step complete", Timestamp: time.Now(),
	})

	cookbookPath := filepath.Join(tmpDir, "cookbook.md")
	if err := os.WriteFile(cookbookPath, []byte("# Cookbook\n\n## Coding\nUse focused tests.\n"), 0644); err != nil {
		t.Fatalf("write cookbook: %v", err)
	}
	reg.Skills = map[string]*config.Skill{
		"o14-skill": {
			ID: "o14-skill", Name: "Coding", Capability: "coding",
			Implementations: map[string]config.SkillImplementation{
				"default": {SystemPrompt: "O14 skill instructions"},
			},
		},
	}
	hub := NewContextHub(reg, nil, es)
	pa := NewPromptAssembler(es, NewResourceLoader(), reg, logger)
	pa.SetMemoryBankStore(mb)
	pa.SetArchive(archive)
	pa.SetRecentInteractions([]store.ToolInteraction{
		{ToolName: "a", Meta: &store.ToolMetadata{DataShape: "daily"}},
		{ToolName: "b", Meta: &store.ToolMetadata{DataShape: "tick"}},
	})

	blocks, err := pa.Build(
		context.Background(), hub,
		&config.Role{ID: "o14-role", Name: "Executor", Instruction: "O14 role instructions", Rules: "O14 hard rule"},
		[]string{"o14-skill"}, "coding",
		&config.ProviderConfig{Model: "o14-model", Provider: "o14-provider", Extra: map[string]any{"cookbook_source": cookbookPath}},
		nil,
		map[string]any{"path": []map[string]any{{"intent": "current coding step", "status": schemas.NodeActive}}},
		[]string{"O14 tool example"}, false,
		[]*schemas.DecisionNode{{Outcome: "O14 precedent", Reasoning: "worked before"}},
		"current coding task", "", "o14-task", "step-1",
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got := make([]string, len(blocks))
	for i, block := range blocks {
		got[i] = block.Category
	}
	want := []string{
		CategoryRoleInstruction, // role instruction
		CategoryRoleInstruction, // privacy-gated UserProfile
		CategoryRoleInstruction, // epistemic grounding
		CategoryPastPrecedents,
		CategoryFailureProfile,
		CategoryCookbook,
		CategorySkills,
		CategoryFewShot,
		CategoryTreePath,
		CategoryConstraints,
		CategoryAntiPattern,
		CategorySimilarTask,
		CategoryActiveContext,
		CategoryArchiveSummary,
		CategoryShapeMismatch,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("O14 block order changed:\n got: %v\nwant: %v", got, want)
	}
	if blocks[9].CacheControl != "ephemeral" || blocks[9].Metadata[GovernanceMarker] != GovTierProtected {
		t.Fatalf("constraints cache marker contract changed: %+v", blocks[9])
	}
	profile := pa.LastProfile()
	if profile == nil || profile.BlockCount != len(blocks) || profile.Model != "o14-model" || profile.Provider != "o14-provider" {
		t.Fatalf("post-budget profile contract changed: %+v", profile)
	}
}

// TestPromptAssembler_O14ConstraintError verifies that a constraints template
// failure preserves Build's existing error prefix and returns no partial blocks.
func TestPromptAssembler_O14ConstraintError(t *testing.T) {
	reg := &config.Registry{PromptOverrides: config.PromptOverrides{OutputContract: "{{"}}
	pa := NewPromptAssembler(nil, nil, reg, nil)
	blocks, err := pa.Build(
		context.Background(), nil, &config.Role{Name: "Executor", Instruction: "role"}, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil, "task", "", "task", "",
	)
	if err == nil || len(err.Error()) < len("template parse error:") || err.Error()[:len("template parse error:")] != "template parse error:" {
		t.Fatalf("Build error = %v, want template parse error", err)
	}
	if blocks != nil {
		t.Fatalf("Build returned partial blocks on template failure: %v", blocks)
	}
}
