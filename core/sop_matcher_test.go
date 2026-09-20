package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// Regression coverage for the 2026-08-06 MatchSOPCandidates performance
// optimization (single-word map lookup vs whole-word substring fallback).
// This touches keyword matching, the same class of logic that caused three
// confirmed regressions in core/tool_router.go (F11/F15/06-30's third
// regression) -- verify the fast path is equivalent to the slow path, not
// just faster.

func newSOPTestRegistry(sops ...*schemas.SOP) *config.Registry {
	reg := &config.Registry{}
	reg.SOPs = make(map[string]*schemas.SOP)
	for _, s := range sops {
		reg.SOPs[s.ID] = s
	}
	return reg
}

func sopWithKeywords(id, description string, keywords ...string) *schemas.SOP {
	return &schemas.SOP{
		ID:          id,
		Description: description,
		Triggers:    []schemas.SOPTrigger{{Keywords: keywords}},
	}
}

func TestMatchSOPCandidates_SingleWordKeyword_FastPathMatches(t *testing.T) {
	reg := newSOPTestRegistry(sopWithKeywords("screenshot_sop", "screenshot diagnostic", "screenshot"))
	got := MatchSOPCandidates(reg, "please take a screenshot of the page", 5)
	if len(got) != 1 || got[0].SOP.ID != "screenshot_sop" {
		t.Fatalf("expected screenshot_sop to match, got %+v", got)
	}
}

func TestMatchSOPCandidates_SingleWordKeyword_NoSubstringFalsePositive(t *testing.T) {
	// "git" as a keyword must NOT match "gitnexus" -- this is exactly the
	// F15/06-30 cross-contamination bug shape, now re-checked against the
	// new fast (map-lookup) path introduced 2026-08-06.
	reg := newSOPTestRegistry(sopWithKeywords("git_sop", "git operations", "git"))
	got := MatchSOPCandidates(reg, "please use gitnexus to search the codebase", 5)
	if len(got) != 0 {
		t.Fatalf("expected no match (gitnexus should not trigger 'git' keyword), got %+v", got)
	}
}

func TestMatchSOPCandidates_MultiWordKeyword_SlowPathStillWorks(t *testing.T) {
	reg := newSOPTestRegistry(sopWithKeywords("market_sop", "daily market report", "daily market report"))
	got := MatchSOPCandidates(reg, "generate the daily market report for today", 5)
	if len(got) != 1 || got[0].SOP.ID != "market_sop" {
		t.Fatalf("expected market_sop multi-word match, got %+v", got)
	}
}

func TestMatchSOPCandidates_MultiWordKeyword_NoPartialMatch(t *testing.T) {
	reg := newSOPTestRegistry(sopWithKeywords("market_sop", "daily market report", "daily market report"))
	got := MatchSOPCandidates(reg, "give me a daily summary, not a market report today", 5)
	if len(got) != 0 {
		t.Fatalf("expected no match (phrase not contiguous), got %+v", got)
	}
}

func TestMatchSOPCandidates_CaseInsensitive_BothPaths(t *testing.T) {
	reg := newSOPTestRegistry(sopWithKeywords("screenshot_sop", "screenshot", "Screenshot", "Daily Market Report"))
	got := MatchSOPCandidates(reg, "SCREENSHOT the DAILY MARKET REPORT page", 5)
	if len(got) != 1 || got[0].Hits != 2 {
		t.Fatalf("expected 1 candidate with 2 hits (case-insensitive, both fast+slow path), got %+v", got)
	}
}
