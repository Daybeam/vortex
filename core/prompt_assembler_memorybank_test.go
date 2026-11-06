package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

func boolPtr(b bool) *bool { return &b }

func TestPromptAssembler_UserProfileInjection_Enabled(t *testing.T) {
	// Set up MemoryBankStore with UserProfile data.
	mbDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(mbDir, nil)
	if err := mbStore.SaveUserProfile(store.UserProfile{
		Expertise:          []string{"backend", "distributed systems"},
		Preferences:        []string{"prefers Go"},
		CommunicationStyle: "concise",
	}); err != nil {
		t.Fatalf("SaveUserProfile failed: %v", err)
	}

	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}
	reg.System.MemoryBankEnabled = boolPtr(true)

	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.SetMemoryBankStore(mbStore)

	blocks, err := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-1", "",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	found := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "User Context") {
			found = true
			if !strings.Contains(b.Text, "backend") {
				t.Errorf("expected expertise 'backend' in User Context, got: %s", b.Text)
			}
			if !strings.Contains(b.Text, "prefers Go") {
				t.Errorf("expected preference 'prefers Go' in User Context, got: %s", b.Text)
			}
			if !strings.Contains(b.Text, "concise") {
				t.Errorf("expected communication style 'concise', got: %s", b.Text)
			}
			break
		}
	}
	if !found {
		t.Fatal("expected User Context block in prompt, not found")
	}
}

func TestPromptAssembler_UserProfileInjection_Disabled(t *testing.T) {
	mbDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(mbDir, nil)
	_ = mbStore.SaveUserProfile(store.UserProfile{
		Expertise: []string{"backend"},
	})

	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}
	reg.System.MemoryBankEnabled = boolPtr(false) // disabled

	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.SetMemoryBankStore(mbStore)

	blocks, _ := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-1", "",
	)

	for _, b := range blocks {
		if strings.Contains(b.Text, "User Context") {
			t.Fatal("expected NO User Context block when MemoryBankEnabled=false")
		}
	}
}

func TestPromptAssembler_UserProfileInjection_NilStore(t *testing.T) {
	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}
	reg.System.MemoryBankEnabled = boolPtr(true)

	pa := NewPromptAssembler(nil, nil, reg, nil)
	// memoryBankStore is nil — no injection

	blocks, _ := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-1", "",
	)

	for _, b := range blocks {
		if strings.Contains(b.Text, "User Context") {
			t.Fatal("expected NO User Context block when memoryBankStore is nil")
		}
	}
}

func TestPromptAssembler_UserProfileInjection_EmptyProfile(t *testing.T) {
	mbDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(mbDir, nil)
	// No UserProfile saved — empty profile

	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}
	reg.System.MemoryBankEnabled = boolPtr(true)

	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.SetMemoryBankStore(mbStore)

	blocks, _ := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-1", "",
	)

	for _, b := range blocks {
		if strings.Contains(b.Text, "User Context") {
			t.Fatal("expected NO User Context block when UserProfile is empty")
		}
	}
}

// --- L1: UserProfile must not leak into delegation envelopes ---
//
// Regression for DELEGATION_HANDOVER_REDESIGN.md §12.6 L1 (P0):
// UserProfile injection at prompt_assembler.go:291 checked only
// MemoryBankEnabled, not DelegationMode. The MemoryBank is a process-global
// singleton (Load() has no owner parameter), so in delegation mode the
// assembled prompt — which becomes the external subagent's envelope — carried
// the operator's (or a prior user's) expertise/preferences/communication-style
// to every SaaS caller. Before fix: "User Context" block present in delegation
// mode. After fix: absent.
func TestPromptAssembler_UserProfileInjection_DelegationMode_Skipped(t *testing.T) {
	mbDir := t.TempDir()
	mbStore := store.NewMemoryBankStore(mbDir, nil)
	if err := mbStore.SaveUserProfile(store.UserProfile{
		Expertise:          []string{"backend"},
		Preferences:        []string{"prefers Go"},
		CommunicationStyle: "concise",
	}); err != nil {
		t.Fatalf("SaveUserProfile failed: %v", err)
	}

	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}
	reg.System.MemoryBankEnabled = boolPtr(true)
	reg.System.DelegationMode = true // ← the bug: this was ignored

	pa := NewPromptAssembler(nil, nil, reg, nil)
	pa.SetMemoryBankStore(mbStore)

	blocks, _ := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-1", "",
	)

	for _, b := range blocks {
		if strings.Contains(b.Text, "User Context") {
			t.Fatal("expected NO User Context block in delegation mode — " +
				"UserProfile must not leak into the external subagent envelope " +
				"(DELEGATION_HANDOVER_REDESIGN.md §12.6 L1)")
		}
	}
}

// --- Epistemic Grounding Rule (Self-Healing §2) ---

func TestPromptAssembler_EpistemicGroundingRule_Enabled(t *testing.T) {
	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}
	reg.System.MemoryBankEnabled = boolPtr(true)

	pa := NewPromptAssembler(nil, nil, reg, nil)

	blocks, err := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-1", "",
	)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	found := false
	for _, b := range blocks {
		if strings.Contains(b.Text, "Epistemic Grounding Rule") {
			found = true
			if !strings.Contains(b.Text, "NOT immutable truth") {
				t.Errorf("expected 'NOT immutable truth' in rule, got: %s", b.Text)
			}
			if !strings.Contains(b.Text, "TRUST THE TOOL") {
				t.Errorf("expected 'TRUST THE TOOL' directive in rule, got: %s", b.Text)
			}
			break
		}
	}
	if !found {
		t.Fatal("expected Epistemic Grounding Rule block when MemoryBankEnabled=true")
	}
}

func TestPromptAssembler_EpistemicGroundingRule_Disabled(t *testing.T) {
	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
	}
	reg.System.MemoryBankEnabled = boolPtr(false)

	pa := NewPromptAssembler(nil, nil, reg, nil)

	blocks, _ := pa.Build(
		context.Background(), nil, nil, nil, "coding",
		&config.ProviderConfig{}, nil, nil, nil, false, nil,
		"test task", "", "task-1", "",
	)

	for _, b := range blocks {
		if strings.Contains(b.Text, "Epistemic Grounding Rule") {
			t.Fatal("expected NO Epistemic Grounding Rule block when MemoryBankEnabled=false")
		}
	}
}
