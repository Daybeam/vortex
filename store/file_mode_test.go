package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestFileFallbackWarning_MatchesWhatNewStoreLeavesUnavailable pins the startup
// warning to what a nil *sql.DB actually costs. The line used to read only
// "Falling back to File storage", which operators correctly took to mean
// "nothing lost", so the SQLite-only areas below went dark at runtime with no
// trace (docs/audit/unwired-code-register.md §O9).
//
// Wire a file backend for one of the named areas and this test fails: update
// the warning in the same change, not later.
func TestFileFallbackWarning_MatchesWhatNewStoreLeavesUnavailable(t *testing.T) {
	root := t.TempDir()
	expDir := t.TempDir()

	s, err := NewStore(root, expDir, "", &config.SystemSettings{}, nil)
	if err != nil {
		t.Fatalf("NewStore in file mode: %v", err)
	}

	if s.DB != nil {
		t.Error("s.DB must be nil in file mode (the warning is only logged then)")
	}
	if s.Config != nil {
		t.Error("s.Config nil expected: role/SOP database sync is skipped in file mode")
	}
	if s.TaskRegistry != nil {
		t.Error("s.TaskRegistry nil expected: task lifecycle state is memory-only in file mode")
	}
	// These keep working on disk, which is why the warning says so out loud.
	if s.Tasks == nil || s.Experience == nil || s.Schedules == nil || s.MemoryBank == nil {
		t.Fatal("file mode must still provide tasks, experience, schedules and memory bank")
	}

	note := FileFallbackWarning(errors.New(" simulated init failure"))
	for _, area := range []string{
		"task lifecycle registry",
		"baseline store",
		"role/SOP database sync",
		"capability profiles",
		"anti-pattern precedents",
		"decision outcomes",
		"semantic search",
		"OAuth tokens",
		"compaction",
	} {
		if !strings.Contains(note, area) {
			t.Errorf("warning omits the SQLite-only area %q", area)
		}
	}
	for _, survivor := range []string{"tasks", "schedules", "memory-bank markdown", "experience store"} {
		if !strings.Contains(note, survivor) {
			t.Errorf("warning must name what file mode keeps, missing %q", survivor)
		}
	}
	if !strings.Contains(note, "simulated init failure") {
		t.Error("warning must carry the original InitDB error")
	}
}

// TestAntiPatternStore_FileModeDoesNotSurviveRestart backs the "anti-pattern
// precedents" entry above: the store has no file backend at all, so with a nil
// backend neither Upsert nor Reload touches disk.
func TestAntiPatternStore_FileModeDoesNotSurviveRestart(t *testing.T) {
	ctx := context.Background()

	s := NewAntiPatternStore(nil, nil)
	s.Upsert(ctx, AntiPatternPrecedent{ID: "ap-file-mode", Category: "timeout", Confidence: 0.9})
	if got := s.QueryByCategory("timeout"); len(got) != 1 {
		t.Fatalf("in-memory Upsert lost: %d rows", len(got))
	}

	// A restart in file mode starts from nothing.
	restarted := NewAntiPatternStore(nil, nil)
	if n := restarted.PrecedentCount(); n != 0 {
		t.Fatalf("file mode must not persist precedents, got %d", n)
	}
}
