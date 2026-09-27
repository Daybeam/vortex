package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseUserProfile_AllSections(t *testing.T) {
	content := `## Preferences
- prefers Python
- uses React

## Expertise
- backend
- data science

## Past Delegations
- Built a REST API with FastAPI
- Analyzed sales data with pandas

## Communication Style
concise
`
	up := parseUserProfile(content)

	if len(up.Preferences) != 2 || up.Preferences[0] != "prefers Python" {
		t.Fatalf("preferences mismatch: %v", up.Preferences)
	}
	if len(up.Expertise) != 2 || up.Expertise[0] != "backend" {
		t.Fatalf("expertise mismatch: %v", up.Expertise)
	}
	if len(up.PastDelegations) != 2 {
		t.Fatalf("past delegations mismatch: %v", up.PastDelegations)
	}
	if up.CommunicationStyle != "concise" {
		t.Fatalf("communication style mismatch: %q", up.CommunicationStyle)
	}
}

func TestParseUserProfile_Empty(t *testing.T) {
	up := parseUserProfile("")
	if len(up.Preferences) != 0 || len(up.Expertise) != 0 || up.CommunicationStyle != "" {
		t.Fatalf("expected empty profile, got: %+v", up)
	}
}

func TestSaveUserProfile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewMemoryBankStore(dir, nil)

	original := UserProfile{
		Preferences:        []string{"prefers Python", "uses React"},
		Expertise:          []string{"backend"},
		PastDelegations:    []string{"Built a REST API"},
		CommunicationStyle: "concise",
	}

	if err := s.SaveUserProfile(original); err != nil {
		t.Fatalf("SaveUserProfile failed: %v", err)
	}

	// Verify file exists.
	if _, err := os.Stat(filepath.Join(s.baseDir, "userProfile.md")); err != nil {
		t.Fatalf("userProfile.md not written: %v", err)
	}

	// Load and verify round-trip.
	mb, err := s.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(mb.UserProfile.Preferences) != 2 || mb.UserProfile.Preferences[0] != "prefers Python" {
		t.Fatalf("preferences round-trip mismatch: %v", mb.UserProfile.Preferences)
	}
	if mb.UserProfile.CommunicationStyle != "concise" {
		t.Fatalf("communication style round-trip mismatch: %q", mb.UserProfile.CommunicationStyle)
	}
}
