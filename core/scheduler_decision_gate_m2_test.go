package core

import (
	"testing"
)

// audit M2 (2026-11-05): regression test for word-boundary fuzzy match.
// The former strings.Contains fallback matched "skip" inside "skipping",
// so a decider response like "I recommend abort because skipping would
// lose progress" returned "skip" (first in iteration order) instead of
// "abort" (the decider's actual intent). containsWord requires whole-word
// boundaries and the caller prefers the longest matching option.

func TestContainsWord_BasicMatches(t *testing.T) {
	cases := []struct {
		s    string
		word string
		want bool
	}{
		// Whole word at various positions
		{"skip", "skip", true},
		{"please skip this", "skip", true},
		{"skip now", "skip", true},
		{"now skip", "skip", true},
		// Word-boundary characters (non-alnum, non-underscore)
		{"skip, abort", "skip", true},
		{"(skip)", "skip", true},
		{"skip.", "skip", true},
		// Substring inside a larger word — the bug
		{"skipping", "skip", false},
		{"skipped", "skip", false},
		{"preskip", "skip", false},
		{"skipper", "skip", false},
		{"my_skip", "skip", false}, // underscore is a word char
		{"skip_var", "skip", false},
		// Empty word never matches
		{"anything", "", false},
		{"", "", false},
		// Word longer than text
		{"ab", "abc", false},
		// Multiple occurrences — first whole-word match wins
		{"skipping skip", "skip", true},
	}
	for _, c := range cases {
		got := containsWord(c.s, c.word)
		if got != c.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", c.s, c.word, got, c.want)
		}
	}
}

func TestContainsWord_FuzzyFallbackDoesNotMatchSubstring(t *testing.T) {
	// Direct regression: "skip" must not match inside "skipping".
	// Before the fix, strings.Contains("skipping", "skip") == true,
	// causing the decider to return "skip" when the text meant "skipping".
	if containsWord("skipping", "skip") {
		t.Error("containsWord matched 'skip' inside 'skipping' — " +
			"word-boundary check is broken")
	}
}

func TestIsWordChar(t *testing.T) {
	wordChars := "abcdefghijklmnopqrstuvwxyz0123456789_"
	for i := 0; i < len(wordChars); i++ {
		if !isWordChar(wordChars[i]) {
			t.Errorf("isWordChar(%q) = false, want true", wordChars[i])
		}
	}
	nonWordChars := " ,.!()-/\\"
	for i := 0; i < len(nonWordChars); i++ {
		if isWordChar(nonWordChars[i]) {
			t.Errorf("isWordChar(%q) = true, want false", nonWordChars[i])
		}
	}
}
