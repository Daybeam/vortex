package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryBankSave_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewMemoryBankStore(dir, nil)

	original := MemoryBank{
		ActiveContext: ActiveContext{
			Goals:         []string{"ship v2.0", "fix auth bug"},
			RecentChanges: []string{"added MemoryBank toggle"},
			OpenQuestions: []string{"should we add TTL?"},
		},
		Decisions: []Decision{
			{Title: "Use SQLite", Decision: "SQLite for local", Rationale: "zero config"},
			{Title: "Skip versioning", Decision: "last-write-wins", Rationale: "YAGNI"},
		},
		ProductContext: "Orchestrator MCP gateway v2.0",
		Progress: Progress{
			Done:    []string{"config toggle", "CRUD API"},
			Current: []string{"sub-agent injection"},
			Next:    []string{"tests", "docs"},
		},
		SystemPatterns: []string{"config-first", "nil-safe"},
		UserProfile: UserProfile{
			Preferences:        []string{"prefers Go", "uses Vim"},
			Expertise:          []string{"backend", "distributed systems"},
			PastDelegations:    []string{"auth refactor"},
			CommunicationStyle: "concise",
		},
	}

	if err := s.Save(original); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("Load returned nil")
	}

	// Verify ActiveContext
	if len(loaded.ActiveContext.Goals) != 2 {
		t.Fatalf("expected 2 goals, got %d", len(loaded.ActiveContext.Goals))
	}
	if loaded.ActiveContext.Goals[0] != "ship v2.0" {
		t.Fatalf("expected first goal 'ship v2.0', got %s", loaded.ActiveContext.Goals[0])
	}
	if len(loaded.ActiveContext.RecentChanges) != 1 {
		t.Fatalf("expected 1 recent change, got %d", len(loaded.ActiveContext.RecentChanges))
	}
	if len(loaded.ActiveContext.OpenQuestions) != 1 {
		t.Fatalf("expected 1 open question, got %d", len(loaded.ActiveContext.OpenQuestions))
	}

	// Verify Decisions
	if len(loaded.Decisions) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(loaded.Decisions))
	}
	if loaded.Decisions[0].Title != "Use SQLite" {
		t.Fatalf("expected first decision title 'Use SQLite', got %s", loaded.Decisions[0].Title)
	}

	// Verify ProductContext
	if loaded.ProductContext != "Orchestrator MCP gateway v2.0" {
		t.Fatalf("expected product context, got %s", loaded.ProductContext)
	}

	// Verify Progress
	if len(loaded.Progress.Done) != 2 {
		t.Fatalf("expected 2 done items, got %d", len(loaded.Progress.Done))
	}
	if len(loaded.Progress.Current) != 1 {
		t.Fatalf("expected 1 current item, got %d", len(loaded.Progress.Current))
	}
	if len(loaded.Progress.Next) != 2 {
		t.Fatalf("expected 2 next items, got %d", len(loaded.Progress.Next))
	}

	// Verify SystemPatterns
	if len(loaded.SystemPatterns) != 2 {
		t.Fatalf("expected 2 system patterns, got %d", len(loaded.SystemPatterns))
	}

	// Verify UserProfile
	if len(loaded.UserProfile.Expertise) != 2 {
		t.Fatalf("expected 2 expertise items, got %d", len(loaded.UserProfile.Expertise))
	}
	if loaded.UserProfile.CommunicationStyle != "concise" {
		t.Fatalf("expected communication style 'concise', got %s", loaded.UserProfile.CommunicationStyle)
	}
}

func TestMemoryBankSave_EmptyMemoryBank(t *testing.T) {
	dir := t.TempDir()
	s := NewMemoryBankStore(dir, nil)

	// Save an empty MemoryBank — should not error.
	if err := s.Save(MemoryBank{}); err != nil {
		t.Fatalf("Save of empty MemoryBank failed: %v", err)
	}

	// Files should exist (even if empty).
	if _, err := os.Stat(filepath.Join(s.baseDir, "activeContext.md")); err != nil {
		t.Fatalf("expected activeContext.md to exist: %v", err)
	}
}

func TestMemoryBankSave_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	s := NewMemoryBankStore(dir, nil)

	// Save initial
	first := MemoryBank{
		ProductContext: "version 1",
		UserProfile:    UserProfile{Expertise: []string{"frontend"}},
	}
	if err := s.Save(first); err != nil {
		t.Fatalf("first Save failed: %v", err)
	}

	// Save updated
	second := MemoryBank{
		ProductContext: "version 2",
		UserProfile:    UserProfile{Expertise: []string{"backend", "devops"}},
	}
	if err := s.Save(second); err != nil {
		t.Fatalf("second Save failed: %v", err)
	}

	loaded, _ := s.Load()
	if loaded.ProductContext != "version 2" {
		t.Fatalf("expected 'version 2', got %s", loaded.ProductContext)
	}
	if len(loaded.UserProfile.Expertise) != 2 {
		t.Fatalf("expected 2 expertise items after overwrite, got %d", len(loaded.UserProfile.Expertise))
	}
}

func TestBoundNegativeAssertion(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		bounded bool // expect a temporal anchor appended
	}{
		{"empty", "", false},
		{"positive", "client wants feature X", false},
		{"negative with anchor", "API does not support Y as of 2026-10-01", false},
		{"negative with because", "cannot use direct DB access because of security policy", false},
		{"negative no anchor", "client does not want dark mode", true},
		{"reject no anchor", "reject all external API calls", true},
		{"unsupported no anchor", "unsupported on Windows", true},
		{"cannot no anchor", "cannot deploy to production", true},
		{"negative with since", "not available since last update", false},
		{"negative with until", "unsupported until v2.0", false},
		{"negative with currently", "currently not supported on ARM", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := boundNegativeAssertion(tt.input)
			if tt.input == "" {
				if result != "" {
					t.Errorf("empty input should return empty, got %q", result)
				}
				return
			}
			hasAnchor := strings.Contains(result, "as of") || strings.Contains(result, "verify before relying")
			if tt.bounded && !hasAnchor {
				t.Errorf("expected temporal anchor appended to %q, got %q", tt.input, result)
			}
			if !tt.bounded && hasAnchor && !strings.Contains(tt.input, "as of") {
				t.Errorf("did not expect anchor appended to %q, got %q", tt.input, result)
			}
		})
	}
}

func TestMemoryBankSave_BoundsNegativeAssertions(t *testing.T) {
	dir := t.TempDir()
	s := NewMemoryBankStore(dir, nil)

	mb := MemoryBank{
		SystemPatterns: []string{
			"config-first",                         // no negative — unchanged
			"do not use direct DB access",          // negative, no anchor — bounded
			"cannot deploy without tests as of v2", // negative, has anchor — unchanged
		},
		Decisions: []Decision{
			{Title: "Auth", Decision: "reject plain HTTP", Rationale: "security"}, // reject — bounded
		},
	}

	if err := s.Save(mb); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Pattern 0: no negative keyword — unchanged.
	if loaded.SystemPatterns[0] != "config-first" {
		t.Errorf("pattern 0 should be unchanged, got %q", loaded.SystemPatterns[0])
	}
	// Pattern 1: negative without anchor — should have temporal suffix.
	if !strings.Contains(loaded.SystemPatterns[1], "verify before relying") {
		t.Errorf("pattern 1 should be bounded, got %q", loaded.SystemPatterns[1])
	}
	// Pattern 2: negative with anchor — unchanged.
	if loaded.SystemPatterns[2] != "cannot deploy without tests as of v2" {
		t.Errorf("pattern 2 should be unchanged (already anchored), got %q", loaded.SystemPatterns[2])
	}
	// Decision: "reject plain HTTP" — should be bounded.
	if !strings.Contains(loaded.Decisions[0].Decision, "verify before relying") {
		t.Errorf("decision should be bounded, got %q", loaded.Decisions[0].Decision)
	}
}
