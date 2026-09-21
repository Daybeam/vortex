package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

// These tests cover two real bugs found and fixed 2026-09-09 in the newly
// (same-day) wired ActiveContextAssembler/resolveFailureModeProfile
// integration into buildSystemPrompt, documented in the 2026-09-09 playbook
// addendum:
//
//  1. Assemble() (the FMC Tier1/2/3 experience-retrieval injection) was
//     nested inside `if similarTaskText != ""`, gating it on an entirely
//     unrelated signal -- a step with zero QuerySimilarPatterns hits but a
//     directly relevant FailureMode precedent would never see it injected.
//  2. resolveFailureModeProfile (FMC Phase 3, GetFailureModeProfile) was
//     defined but never called from buildSystemPrompt at all.

// TestSpawner_ActiveContextAssembler_FiresWithoutSimilarTaskPatterns is the
// regression test for bug #1: seeds a failure-mode-labeled ExperienceNode
// directly (bypassing QuerySimilarPatterns/TaskPatterns entirely, so
// resolveSimilarTaskExperience is guaranteed to return "") and confirms the
// Assemble() output still appears in the built prompt.
func TestSpawner_ActiveContextAssembler_FiresWithoutSimilarTaskPatterns(t *testing.T) {
	tmpDir := t.TempDir()
	ts := store.NewTaskStore(store.NewFileTaskBackend(tmpDir))
	es, err := store.NewExperienceStore(tmpDir, ts, &config.SystemSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}
	// Deliberately do NOT register any TaskPatterns -- QuerySimilarPatterns
	// must return empty, so resolveSimilarTaskExperience("coding") == "".
	es.Nodes["n1"] = &store.ExperienceNode{
		NodeID:      "n1",
		Capability:  "coding",
		Outcome:     "failure",
		FailureMode: "timeout_exceeded",
		Strategy:    "ran a long-lived shell command without a timeout",
		Critique:    "always pass an explicit timeout to long-running subprocess calls",
	}

	reg := &config.Registry{}
	logger, _ := NewLogger(t.TempDir(), &config.SystemSettings{})
	spawner := NewSpawner(reg, ts, es, logger, nil, "outputs")

	blocks, err := spawner.buildSystemPrompt(
		nil, &config.Role{ID: "r1", Name: "test"}, nil, "coding",
		&config.ProviderConfig{Model: "test-model"}, []string{}, nil, nil, false, nil,
		"do the coding task", "operation timeout after 30s",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	foundSimilarTask := false
	foundLearnedExperience := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "Relevant Historical Task Patterns") {
			foundSimilarTask = true
		}
		if strings.Contains(b.Text, "[LEARNED EXPERIENCE PRECEDENT]") {
			foundLearnedExperience = true
			if !strings.Contains(b.Text, "timeout_exceeded") {
				t.Errorf("expected the FailureMode label in the injected block, got: %s", b.Text)
			}
		}
	}
	if foundSimilarTask {
		t.Error("resolveSimilarTaskExperience should have found nothing (no TaskPatterns registered) -- test setup assumption violated")
	}
	if !foundLearnedExperience {
		t.Error("Assemble()'s [LEARNED EXPERIENCE PRECEDENT] block must appear even when there are zero similar task patterns -- it must not be gated on similarTaskText")
	}
}

// TestSpawner_FailureModeProfileInjection is the regression test for bug #2:
// seeds two failure nodes for the same model with different failure modes
// and confirms resolveFailureModeProfile's "Known Weak Spots" summary
// appears in the built prompt, sorted by frequency.
func TestSpawner_FailureModeProfileInjection(t *testing.T) {
	tmpDir := t.TempDir()
	ts := store.NewTaskStore(store.NewFileTaskBackend(tmpDir))
	es, err := store.NewExperienceStore(tmpDir, ts, &config.SystemSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}
	es.Nodes["n1"] = &store.ExperienceNode{NodeID: "n1", ModelID: "test-model", Outcome: "failure", FailureMode: "tool_selection_error"}
	es.Nodes["n2"] = &store.ExperienceNode{NodeID: "n2", ModelID: "test-model", Outcome: "failure", FailureMode: "tool_selection_error"}
	es.Nodes["n3"] = &store.ExperienceNode{NodeID: "n3", ModelID: "test-model", Outcome: "failure", FailureMode: "rate_limit_hit"}
	// A different model's failures must not leak into test-model's profile.
	es.Nodes["n4"] = &store.ExperienceNode{NodeID: "n4", ModelID: "other-model", Outcome: "failure", FailureMode: "auth_permission_denied"}

	reg := &config.Registry{}
	logger, _ := NewLogger(t.TempDir(), &config.SystemSettings{})
	spawner := NewSpawner(reg, ts, es, logger, nil, "outputs")

	blocks, err := spawner.buildSystemPrompt(
		nil, &config.Role{ID: "r1", Name: "test"}, nil, "coding",
		&config.ProviderConfig{Model: "test-model"}, []string{}, nil, nil, false, nil,
		"do the coding task", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	found := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "Known Weak Spots for test-model") {
			found = true
			if !strings.Contains(b.Text, "tool_selection_error (2 prior occurrence(s))") {
				t.Errorf("expected tool_selection_error listed with count 2, got: %s", b.Text)
			}
			if !strings.Contains(b.Text, "rate_limit_hit (1 prior occurrence(s))") {
				t.Errorf("expected rate_limit_hit listed with count 1, got: %s", b.Text)
			}
			if strings.Contains(b.Text, "auth_permission_denied") {
				t.Errorf("other-model's failure modes must not leak into test-model's profile, got: %s", b.Text)
			}
		}
	}
	if !found {
		t.Fatal("expected resolveFailureModeProfile's 'Known Weak Spots' block to be injected into the prompt")
	}
}

// TestSpawner_FailureModeProfileInjection_NoDataIsSilent confirms the
// helper is a graceful no-op (doesn't inject an empty/misleading block)
// when the store has no failure data for this model at all.
func TestSpawner_FailureModeProfileInjection_NoDataIsSilent(t *testing.T) {
	tmpDir := t.TempDir()
	ts := store.NewTaskStore(store.NewFileTaskBackend(tmpDir))
	es, err := store.NewExperienceStore(tmpDir, ts, &config.SystemSettings{}, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}

	reg := &config.Registry{}
	logger, _ := NewLogger(t.TempDir(), &config.SystemSettings{})
	spawner := NewSpawner(reg, ts, es, logger, nil, "outputs")

	blocks, err := spawner.buildSystemPrompt(
		nil, &config.Role{ID: "r1", Name: "test"}, nil, "coding",
		&config.ProviderConfig{Model: "unseen-model"}, []string{}, nil, nil, false, nil,
		"do the coding task", "",
	)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}
	for _, b := range blocks {
		if strings.Contains(b.Text, "Known Weak Spots") {
			t.Errorf("did not expect a Known Weak Spots block with zero data for this model, got: %s", b.Text)
		}
	}
}
