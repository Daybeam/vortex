package core

// input_normalization.go — L1 input normalization + injection marking
// (design: docs/completed/2026-10-22/L1_INPUT_NORMALIZATION_DESIGN.md).
//
// Threat: instructions smuggled into tool results via encoding layers
// (base64 / HTML entities / URL percent / \x escapes / zero-width runes).
// A plain-text pattern scan never sees them; the model decodes and obeys.
//
// Unit: NormalizeInput builds a decode-to-fixpoint VIEW; the injection
// pattern list is matched against that view; a hit wraps the ORIGINAL
// content in <untrusted_data> (soft marking, matching the existing L2
// philosophy — content still flows, delimited as data).

import (
	"encoding/base64"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// maxNormalizeBytes bounds normalization work (DoS guard). Larger inputs
	// skip the injection view and pass through redaction only.
	maxNormalizeBytes = 1 << 20
	// maxDecodePasses caps the decode-to-fixpoint loop (layered encodings).
	maxDecodePasses = 4
)

// zeroWidthOrBidi reports runes that are invisible / direction-controlling and
// therefore usable to split keywords ("ig\u200Bnore") or reorder text.
func zeroWidthOrBidi(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, // zero-width space..RLM
		r >= 0x202A && r <= 0x202E, // bidi embedding/override
		r >= 0x2066 && r <= 0x2069, // bidi isolates
		r == 0xFEFF, r == 0x2060:   // BOM, word joiner
		return true
	}
	return false
}

var base64RunRe = regexp.MustCompile(`[A-Za-z0-9+/]{16,}={0,3}`)

// injectionPatterns is a deliberately tight list (EN + ZH) of unambiguous
// instruction-hijack phrasings. Loose stems ("you are now", "new role") are
// excluded on purpose — false positives on legitimate content cost more than
// the marginal recall they add; the defense is soft marking, not blocking.
var injectionPatterns = []string{
	"ignore previous instructions",
	"ignore all previous instructions",
	"disregard previous instructions",
	"disregard the above instructions",
	"forget your instructions",
	"forget previous instructions",
	"override your instructions",
	"override all previous instructions",
	"忽略之前的指令",
	"忽略以上指令",
	"无视之前的指令",
	"developer mode",
	"jailbreak",
}

// ContainsInjectionPatterns reports whether any known hijack phrasing appears
// in s (case-insensitive substring).
func ContainsInjectionPatterns(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, p := range injectionPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// NormalizeInput returns a normalized VIEW of s for detection purposes:
// invisible/control runes stripped, HTML entities / URL percent / \xHH and
// \uHHHH escapes decoded, and printable-looking base64 runs decoded —
// iterated to a fixpoint (≤ maxDecodePasses). It never mutates stored
// content; callers only pattern-match against the view.
func NormalizeInput(s string) string {
	if s == "" {
		return s
	}
	cur := s
	for i := 0; i < maxDecodePasses; i++ {
		next := normalizeOnce(cur)
		if next == cur {
			break
		}
		cur = next
	}
	return cur
}

func normalizeOnce(s string) string {
	// 1. Strip zero-width / bidi control runes (word-splitting evasion).
	s = strings.Map(func(r rune) rune {
		if zeroWidthOrBidi(r) {
			return -1
		}
		return r
	}, s)
	// 2. HTML entities (&#x69; / &#105; / &lt; ...) — stdlib.
	s = html.UnescapeString(s)
	// 3. URL percent-encoding (%69%67...) — guarded, best effort.
	if strings.ContainsRune(s, '%') {
		if u, err := url.QueryUnescape(s); err == nil {
			s = u
		}
	}
	// 4. \xHH and \uHHHH escapes.
	s = unescapeHexRunes(s)
	// 5. Base64 runs, only when they decode to mostly-printable text.
	s = decodePrintableBase64Runs(s)
	return s
}

// unescapeHexRunes replaces literal \xHH and \uHHHH sequences with their runes.
func unescapeHexRunes(s string) string {
	if !strings.Contains(s, `\x`) && !strings.Contains(s, `\u`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'x':
				if r, n, ok := hexRune(s[i+2:], 2); ok {
					b.WriteRune(r)
					i += 2 + n
					continue
				}
			case 'u':
				if r, n, ok := hexRune(s[i+2:], 4); ok {
					b.WriteRune(r)
					i += 2 + n
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// hexRune parses n hex digits into a rune; n is 2 (\xHH) or 4 (\uHHHH).
func hexRune(s string, n int) (rune, int, bool) {
	if len(s) < n {
		return 0, 0, false
	}
	v := 0
	for i := 0; i < n; i++ {
		d, ok := hexVal(s[i])
		if !ok {
			return 0, 0, false
		}
		v = v<<4 | d
	}
	return rune(v), n, true
}

func hexVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// decodePrintableBase64Runs decodes base64-looking runs ONLY when the decoded
// bytes are valid UTF-8, ≥8 bytes, and ≥90% printable — binary payloads
// (images, hashes, keys) stay untouched, and accidental English words that
// happen to match the charset decode to garbage that fails the guard.
func decodePrintableBase64Runs(s string) string {
	if !base64RunRe.MatchString(s) {
		return s
	}
	// Skip long JSON-ish strings cheaply: runs inside them decode to garbage
	// anyway; the guard below is the real referee.
	return base64RunRe.ReplaceAllStringFunc(s, func(run string) string {
		decoded, ok := tryBase64(run)
		if !ok {
			return run
		}
		return string(decoded)
	})
}

func tryBase64(run string) ([]byte, bool) {
	encodings := []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding}
	for _, enc := range encodings {
		if dec, err := enc.DecodeString(run); err == nil {
			if mostlyPrintable(dec) {
				return dec, true
			}
			return nil, false // decodable but binary — leave it alone
		}
	}
	return nil, false
}

func mostlyPrintable(b []byte) bool {
	if len(b) < 8 || !utf8.Valid(b) {
		return false
	}
	printable := 0
	for _, r := range string(b) {
		if unicode.IsPrint(r) || r == '\n' || r == '\r' || r == '\t' {
			printable++
		}
	}
	return printable*10 >= utf8.RuneCountInString(string(b))*9
}

const untrustedToolResultTag = `<untrusted_data from="tool_result" reason="injection_suspected">`

// redactAndMarkUntrusted replaces the bare `md = RedactEgress(md)` step at the
// tool-result→context seam (spawner_tool_exec.go, local + remote branches):
// egress redaction first (secrets stripped), then — if the NORMALIZED view of
// the result matches known hijack phrasings (directly or via any encoding
// layer) — the original content is wrapped as untrusted data so the model
// treats it as data, not instructions. Soft defense: content still flows.
func redactAndMarkUntrusted(md string) string {
	md = RedactEgress(md)
	if md == "" || len(md) > maxNormalizeBytes {
		return md
	}
	if strings.Contains(md, untrustedToolResultTag) {
		return md // already marked — no double wrap
	}
	if ContainsInjectionPatterns(NormalizeInput(md)) {
		return fmt.Sprintf("%s\n%s\n</untrusted_data>", untrustedToolResultTag, md)
	}
	return md
}
