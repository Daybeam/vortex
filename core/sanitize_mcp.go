package core

import (
	"log/slog"
	"reflect"
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

// SanitizeMCPToolSchema recursively strips invisible Unicode from every string
// leaf in an input schema (title, description, enum values, default, property
// keys, examples). Returns a deep-copied schema; the input is not mutated.
func SanitizeMCPToolSchema(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		out[SanitizeMCPToolText(k)] = sanitizeMCPToolValue(v)
	}
	return out
}

func sanitizeMCPToolValue(v any) any {
	switch tv := v.(type) {
	case string:
		return SanitizeMCPToolText(tv)
	case map[string]any:
		return SanitizeMCPToolSchema(tv)
	case []any:
		out := make([]any, len(tv))
		for i, e := range tv {
			out[i] = sanitizeMCPToolValue(e)
		}
		return out
	default:
		return v
	}
}

// VerifyApprovalFidelity checks whether raw MCP tool metadata contains any
// invisible Unicode characters. Returns true if the raw strings are clean
// (sanitization is a no-op), false if concealment was detected (fail-closed:
// the tool should be rejected, not silently stripped).
//
// See MCP_UNICODE_TAG_SANITIZATION_DESIGN.md §3.5.
func VerifyApprovalFidelity(rawName, rawDesc string) bool {
	return SanitizeMCPToolText(rawName) == rawName &&
		SanitizeMCPToolText(rawDesc) == rawDesc
}

// VerifySchemaFidelity checks whether a raw input schema contains any invisible
// Unicode characters in its string leaves (keys, values, enum elements).
// Returns true if the schema is clean, false if concealment was detected.
func VerifySchemaFidelity(schema map[string]any) bool {
	return reflect.DeepEqual(SanitizeMCPToolSchema(schema), schema)
}
