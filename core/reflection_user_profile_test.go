package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func TestExtractUserProfile_ExtractsFromTaskGraph(t *testing.T) {
	dir := t.TempDir()
	mbStore := store.NewMemoryBankStore(dir, nil)

	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"backend-dev": {BaseCapability: "backend"},
		},
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{},
	}

	re := NewReflectionEngine(nil, nil, reg, nil, nil)
	re.SetMemoryBankStore(mbStore)

	graph := &schemas.TaskGraph{
		TaskID: "task-profile-test",
		Steps: map[string]*schemas.Step{
			"s1": {
				ID:     "s1",
				RoleID: "backend-dev",
				Status: schemas.StepOK,
				Task:   "Build a Python REST API with FastAPI",
			},
			"s2": {
				ID:     "s2",
				RoleID: "backend-dev",
				Status: schemas.StepOK,
				Task:   "Create React frontend components",
			},
			"s3": {
				ID:     "s3",
				Status: schemas.StepPending, // should be skipped
				Task:   "pending work",
			},
		},
	}

	re.extractUserProfile(context.Background(), graph)

	mb, _ := mbStore.Load()
	up := mb.UserProfile

	// Should extract "backend" expertise from role capability.
	if !containsStr(up.Expertise, "backend") {
		t.Fatalf("expected 'backend' in expertise, got: %v", up.Expertise)
	}

	// Should extract "prefers Python" from task description.
	if !containsStr(up.Preferences, "prefers Python") {
		t.Fatalf("expected 'prefers Python' in preferences, got: %v", up.Preferences)
	}

	// Should extract "uses React" from task description.
	if !containsStr(up.Preferences, "uses React") {
		t.Fatalf("expected 'uses React' in preferences, got: %v", up.Preferences)
	}

	// Should have 2 past delegations (s3 is pending, skipped).
	if len(up.PastDelegations) != 2 {
		t.Fatalf("expected 2 past delegations, got %d: %v", len(up.PastDelegations), up.PastDelegations)
	}
}

func TestExtractUserProfile_NilMbStore_NoOp(t *testing.T) {
	re := NewReflectionEngine(nil, nil, nil, nil, nil)
	// mbStore is nil — should return without error.
	re.extractUserProfile(context.Background(), &schemas.TaskGraph{
		TaskID: "task-noop",
		Steps:  map[string]*schemas.Step{"s1": {ID: "s1", Status: schemas.StepOK, Task: "test"}},
	})
}

func TestExtractUserProfile_DedupAndMerge(t *testing.T) {
	dir := t.TempDir()
	mbStore := store.NewMemoryBankStore(dir, nil)

	// Pre-populate with existing profile.
	_ = mbStore.SaveUserProfile(store.UserProfile{
		Expertise:       []string{"backend"},
		Preferences:     []string{"prefers Python"},
		PastDelegations: []string{"existing task"},
	})

	reg := &config.Registry{
		Roles:     map[string]*config.Role{"backend-dev": {BaseCapability: "backend"}},
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{},
	}

	re := NewReflectionEngine(nil, nil, reg, nil, nil)
	re.SetMemoryBankStore(mbStore)

	// Run extraction with a task that would add "backend" again (dedup test)
	// and a new task (merge test).
	graph := &schemas.TaskGraph{
		TaskID: "task-dedup",
		Steps: map[string]*schemas.Step{
			"s1": {
				ID:     "s1",
				RoleID: "backend-dev",
				Status: schemas.StepOK,
				Task:   "Build a Python REST API",
			},
		},
	}

	re.extractUserProfile(context.Background(), graph)

	mb, _ := mbStore.Load()
	up := mb.UserProfile

	// "backend" should appear only once (dedup).
	count := 0
	for _, e := range up.Expertise {
		if e == "backend" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 'backend' once (dedup), got %d times: %v", count, up.Expertise)
	}

	// "prefers Python" should appear only once (dedup).
	count = 0
	for _, p := range up.Preferences {
		if p == "prefers Python" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 'prefers Python' once (dedup), got %d times: %v", count, up.Preferences)
	}

	// "existing task" should still be present (merge, not replace).
	if !containsStr(up.PastDelegations, "existing task") {
		t.Fatalf("expected 'existing task' to survive merge, got: %v", up.PastDelegations)
	}
}

func TestAppendUnique_Dedup(t *testing.T) {
	list := []string{"a", "b"}
	list = appendUnique(list, "a", 10) // duplicate
	if len(list) != 2 {
		t.Fatalf("expected 2 items (dedup), got %d: %v", len(list), list)
	}
}

func TestAppendUnique_Cap(t *testing.T) {
	list := []string{"a", "b", "c"}
	list = appendUnique(list, "d", 3) // at cap
	if len(list) != 3 {
		t.Fatalf("expected 3 items (capped), got %d: %v", len(list), list)
	}
}

func TestAppendUnique_NewItem(t *testing.T) {
	list := []string{"a", "b"}
	list = appendUnique(list, "c", 10)
	if len(list) != 3 || list[2] != "c" {
		t.Fatalf("expected 3 items with 'c' appended, got: %v", list)
	}
}
