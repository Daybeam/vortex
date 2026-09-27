package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/store"
)

func TestBuildSystem_UserProfileInjected(t *testing.T) {
	dir := t.TempDir()
	mbStore := store.NewMemoryBankStore(dir, nil)

	// Save a user profile.
	err := mbStore.SaveUserProfile(store.UserProfile{
		Preferences:        []string{"prefers Python", "uses React"},
		Expertise:          []string{"backend", "data science"},
		CommunicationStyle: "concise",
	})
	if err != nil {
		t.Fatalf("SaveUserProfile failed: %v", err)
	}

	h := &ChatHarness{
		MemoryBank: mbStore,
	}

	system := h.buildSystem("test query", "task-1")

	// Verify user profile is injected.
	if !strings.Contains(system, "## User Profile") {
		t.Fatal("system prompt should contain '## User Profile' section")
	}
	if !strings.Contains(system, "prefers Python") {
		t.Fatal("system prompt should contain user preferences")
	}
	if !strings.Contains(system, "backend, data science") {
		t.Fatal("system prompt should contain user expertise")
	}
	if !strings.Contains(system, "Communication style: concise") {
		t.Fatal("system prompt should contain communication style")
	}
}

func TestBuildSystem_NoUserProfile_NoSection(t *testing.T) {
	dir := t.TempDir()
	mbStore := store.NewMemoryBankStore(dir, nil)

	h := &ChatHarness{
		MemoryBank: mbStore,
	}

	system := h.buildSystem("test query", "task-1")

	// When no user profile is set, the section should not appear.
	if strings.Contains(system, "## User Profile") {
		t.Fatal("system prompt should NOT contain '## User Profile' when profile is empty")
	}
}
