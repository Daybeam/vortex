package providers

// estimateTokens returns a rough token-count estimate for text when no
// provider-native token-counting API is available (or when a real API call
// fails and we need a safe fallback rather than hard-failing).
//
// FIX (2026-07-08): the previous heuristic used by most providers was a
// flat len(text)/4. Go's len() on a string returns *byte* length, not rune
// count, which means the old heuristic's behavior on CJK text was an
// accident of UTF-8 encoding width rather than anything principled: common
// CJK characters are 3 bytes each, so len(text)/4 worked out to roughly
// 0.75 tokens/char for pure-CJK text -- which happens to land in a
// plausible range (mainstream tokenizers give roughly 0.5-0.7 tokens/char
// for CJK), but only by coincidence. That coincidence doesn't hold up for:
//   - mixed-language text, where the blended byte-length/4 estimate has no
//     principled relationship to the actual token mix
//   - other multi-byte-but-simple scripts (e.g. many 4-byte emoji/symbol
//     code points), which would be over-counted proportionally to their
//     encoded byte width rather than their actual tokenization cost
//
// This version counts runes and classifies CJK vs. non-CJK explicitly, so
// the estimate is driven by an actual (if still rough) per-script model
// instead of an incidental byte-width artifact. It is not meaningfully
// more or less accurate than the old heuristic for pure Chinese text
// specifically (both land in a plausible ballpark) -- the real win is that
// it's now correct *for the right reason*, and generalizes better to mixed
// or non-CJK multi-byte content.
//
// This is still an estimate, not a real tokenizer -- for providers with a
// real token-counting API (see AnthropicProvider/GeminiProvider.CountTokens)
// we prefer that and only fall back to this when the API is unavailable or
// errors.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	var cjkRunes, otherRunes int
	for _, r := range text {
		if isCJK(r) {
			cjkRunes++
		} else {
			otherRunes++
		}
	}
	// CJK: empirically closer to 1.5-2 characters per token across
	// mainstream BPE tokenizers (cl100k_base, Claude's tokenizer, etc).
	// We use 1.7 as a middle-ground estimate.
	cjkTokens := float64(cjkRunes) / 1.7
	// Non-CJK: the standard ~4 characters per token rule of thumb for
	// English/Latin-script text.
	otherTokens := float64(otherRunes) / 4.0

	total := int(cjkTokens + otherTokens)
	if total == 0 {
		// Any non-empty text costs at least one token.
		total = 1
	}
	return total
}

// isCJK reports whether r falls in one of the common CJK Unicode blocks
// (Chinese, Japanese Hiragana/Katakana, Korean Hangul, and the main CJK
// Unified Ideographs Extension A block). This is intentionally a coarse,
// fast range check rather than a full Unicode script classification --
// good enough for a token-count estimate, not meant to be exhaustive.
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs
		return true
	case r >= 0x3400 && r <= 0x4DBF: // CJK Unified Ideographs Extension A
		return true
	case r >= 0x3040 && r <= 0x309F: // Hiragana
		return true
	case r >= 0x30A0 && r <= 0x30FF: // Katakana
		return true
	case r >= 0xAC00 && r <= 0xD7A3: // Hangul Syllables
		return true
	default:
		return false
	}
}
