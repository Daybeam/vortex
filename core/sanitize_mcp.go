package core

import (
	"log/slog"
	"strings"
	"unicode"
)

// SanitizeMCPToolText strips invisible Unicode characters from MCP tool
// metadata (name, description) to eliminate the Approval-View Fidelity Gap
// and prevent hidden prompt injections via Unicode TAG blocks and zero-width
// characters (arXiv:2607.05744v1).
//
// Removed character classes:
//  1. Unicode TAG Block (U+E0000–U+E007F) — can encode hidden ASCII payload.
//  2. Zero-width / invisible formatting: U+200B (ZWSP), U+200C (ZWNJ),
//     U+200D (ZWJ), U+FEFF (BOM), U+2060 (WJ), U+00AD (SHY).
//  3. Control characters (unicode.IsControl) except \t, \n, \r which are
//     legitimate whitespace in tool descriptions.
//
// The function is pure: no side effects, no allocation beyond the result.
// Normal ASCII and visible Unicode (CJK, emoji, accented letters) pass through
// unchanged.
func SanitizeMCPToolText(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))

	for _, r := range s {
		// 1. Drop Unicode TAG Block (U+E0000 to U+E007F).
		if r >= 0xE0000 && r <= 0xE007F {
			continue
		}
		// 2. Drop zero-width / invisible formatting characters.
		switch r {
		case 0x200B, 0x200C, 0x200D, 0xFEFF, 0x2060, 0x00AD:
			continue
		}
		// 3. Drop control characters except standard whitespace.
		if unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' {
			continue
		}
		sb.WriteRune(r)
	}

	return sb.String()
}

// sanitizeMCPToolField sanitizes s and logs a security warning if any
// prohibited characters were removed. mcpID and field identify the source
// for the audit log. This wrapper keeps the call sites at one line each.
func sanitizeMCPToolField(s, mcpID, field string) string {
	clean := SanitizeMCPToolText(s)
	if clean != s {
		slog.Warn("MCP tool metadata contained prohibited invisible characters (TAG block / zero-width / control); stripped",
			"mcp", mcpID, "field", field)
	}
	return clean
}
