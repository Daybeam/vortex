package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// --- VLM Image Dedup (Self-Healing §1a) ---

func TestDedupAttachments_NoDuplicates(t *testing.T) {
	existing := []schemas.Attachment{{Path: "/a.png", MimeType: "image/png"}}
	incoming := []schemas.Attachment{{Path: "/b.png", MimeType: "image/png"}, {Path: "/c.png", MimeType: "image/png"}}
	result := dedupAttachments(existing, incoming)
	if len(result) != 3 {
		t.Fatalf("no dupes: expected 3, got %d", len(result))
	}
}

func TestDedupAttachments_SkipsDuplicatePath(t *testing.T) {
	existing := []schemas.Attachment{{Path: "/screenshot.png", MimeType: "image/png"}}
	incoming := []schemas.Attachment{{Path: "/screenshot.png", MimeType: "image/png"}}
	result := dedupAttachments(existing, incoming)
	if len(result) != 1 {
		t.Fatalf("dup path: expected 1, got %d", len(result))
	}
}

func TestDedupAttachments_MixedPaths(t *testing.T) {
	existing := []schemas.Attachment{{Path: "/a.png"}, {Path: "/b.png"}}
	incoming := []schemas.Attachment{{Path: "/b.png"}, {Path: "/c.png"}, {Path: "/a.png"}, {Path: "/d.png"}}
	result := dedupAttachments(existing, incoming)
	if len(result) != 4 {
		t.Fatalf("mixed: expected 4 (a,b,c,d), got %d", len(result))
	}
	got := make(map[string]bool)
	for _, r := range result {
		got[r.Path] = true
	}
	for _, expected := range []string{"/a.png", "/b.png", "/c.png", "/d.png"} {
		if !got[expected] {
			t.Errorf("expected %s in result", expected)
		}
	}
}

func TestDedupAttachments_EmptyIncoming(t *testing.T) {
	existing := []schemas.Attachment{{Path: "/a.png"}}
	result := dedupAttachments(existing, nil)
	if len(result) != 1 {
		t.Fatalf("empty incoming: expected 1, got %d", len(result))
	}
}

func TestDedupAttachments_EmptyExisting(t *testing.T) {
	incoming := []schemas.Attachment{{Path: "/a.png"}, {Path: "/b.png"}}
	result := dedupAttachments(nil, incoming)
	if len(result) != 2 {
		t.Fatalf("empty existing: expected 2, got %d", len(result))
	}
}

// --- VLM Image Token Estimation (§1b) ---

func TestEstimateImageTokens_Empty(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	if got := e.EstimateImageTokens(nil); got != 0 {
		t.Errorf("empty: expected 0, got %d", got)
	}
}

func TestEstimateImageTokens_KnownSize(t *testing.T) {
	tmpDir := t.TempDir()
	// Create a 7500-byte file → 7500/750 = 10 tokens.
	p := filepath.Join(tmpDir, "screenshot.png")
	if err := os.WriteFile(p, make([]byte, 7500), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := NewPromptBudgetEnforcer(nil)
	got := e.EstimateImageTokens([]schemas.Attachment{{Path: p, MimeType: "image/png"}})
	if got != 10 {
		t.Errorf("7500 bytes: expected 10 tokens, got %d", got)
	}
}

func TestEstimateImageTokens_SmallFileMinimum(t *testing.T) {
	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "tiny.png")
	if err := os.WriteFile(p, []byte{0x89, 0x50}, 0644); err != nil { // 2 bytes
		t.Fatalf("WriteFile: %v", err)
	}
	e := NewPromptBudgetEnforcer(nil)
	got := e.EstimateImageTokens([]schemas.Attachment{{Path: p}})
	if got != 1 { // minimum 1 token
		t.Errorf("2 bytes: expected 1 token (minimum), got %d", got)
	}
}

func TestEstimateImageTokens_MissingFile(t *testing.T) {
	e := NewPromptBudgetEnforcer(nil)
	got := e.EstimateImageTokens([]schemas.Attachment{{Path: "/nonexistent/path.png"}})
	if got != 0 {
		t.Errorf("missing file: expected 0, got %d", got)
	}
}

func TestEstimateImageTokens_MultipleFiles(t *testing.T) {
	tmpDir := t.TempDir()
	p1 := filepath.Join(tmpDir, "a.png")
	p2 := filepath.Join(tmpDir, "b.png")
	os.WriteFile(p1, make([]byte, 7500), 0644) // 10 tokens
	os.WriteFile(p2, make([]byte, 1500), 0644) // 2 tokens
	e := NewPromptBudgetEnforcer(nil)
	got := e.EstimateImageTokens([]schemas.Attachment{{Path: p1}, {Path: p2}})
	if got != 12 {
		t.Errorf("multiple: expected 12 tokens (10+2), got %d", got)
	}
}
