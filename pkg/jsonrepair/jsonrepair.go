// Package jsonrepair provides best-effort repair of JSON text that was
// truncated by LLM max_tokens limits.
//
// The two strategies mirror the approaches used by agnes-ai-studio's
// _repair_truncated_json (Python), but reimplemented in Go and extended
// with a TrimIncomplete mode that is more appropriate for JSON arrays of
// homogeneous objects (e.g. storyboard shots).
//
// Usage:
//
//	repaired, ok := jsonrepair.Repair(truncatedText)
//	clean, ok := jsonrepair.TrimIncomplete(truncatedText)
package jsonrepair

import (
	"encoding/json"
	"strings"
)

// Repair attempts to fix truncated JSON by closing unclosed strings and
// bracket pairs. It returns the repaired string and true if the result
// parses as valid JSON; otherwise it returns the original text and false.
//
// Strategy (bracket-stack, ported from agnes-ai-studio):
//  1. Track open/close of { } [ ] and " " with escape awareness.
//  2. If the text ends inside a string, close the string first.
//  3. Compute the stack of unclosed brackets and append closers in reverse.
//  4. Try json.Unmarshal on the result; return on success.
//  5. If that fails, try a more aggressive approach: find the last
//     "complete value" position (a position followed by , } or ]) and
//     truncate there before closing brackets.
func Repair(s string) (string, bool) {
	// Fast path: already valid JSON.
	if isValid(s) {
		return s, true
	}

	// Strategy 1: close at end.
	repaired := closeAtEnd(s)
	if isValid(repaired) {
		return repaired, true
	}

	// Strategy 2: truncate at last complete value, then close.
	repaired = closeAtLastComplete(s)
	if isValid(repaired) {
		return repaired, true
	}

	return s, false
}

// TrimIncomplete is designed for JSON arrays of homogeneous objects (e.g.
// LLM-generated storyboard shots). Instead of trying to salvage a
// half-finished object at the end, it discards the trailing incomplete
// object entirely and closes the array.
//
// Example:
//
//	`[{"i":1},{"i":2},{"i":3,"n` → `[{"i":1},{"i":2}]`
//
// This is safer than Repair for array data because partial objects are
// usually missing required fields and would fail downstream validation.
func TrimIncomplete(s string) (string, bool) {
	// Fast path: already valid JSON.
	if isValid(s) {
		return s, true
	}

	s = strings.TrimSpace(s)

	// Find the array start position (the first '['), not just whether the
	// text starts with '['. This handles {"shots":[...] } wrappers.
	arrStart := strings.Index(s, "[")
	if arrStart < 0 {
		// No array found — fall back to Repair.
		return Repair(s)
	}

	// Walk the string tracking bracket depth and string state.
	// We look for "safe truncation points": positions right after a } that
	// closes an object at the array level (depth == arrStartDepth+1).
	arrStartDepth := 0
	for i := 0; i < arrStart; i++ {
		if s[i] == '{' || s[i] == '[' {
			arrStartDepth++
		} else if s[i] == '}' || s[i] == ']' {
			arrStartDepth--
		}
	}
	arrayLevel := arrStartDepth + 1 // depth when we are inside the array

	depth := arrStartDepth // start counting from the outer brackets already open
	inString := false
	escaped := false
	lastSafeEnd := -1 // position of the last ] or , at array level that we can truncate after

	for i := arrStart; i < len(s); i++ {
		c := s[i]

		if escaped {
			escaped = false
			continue
		}
		if inString {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}

		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			// When we return to array level after closing an object,
			// mark the next comma or close-bracket as a safe point.
			if depth == arrayLevel && c == '}' {
				// Look ahead for , or ]
				j := i + 1
				for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
					j++
				}
				if j < len(s) && (s[j] == ',' || s[j] == ']') {
					lastSafeEnd = j // position of the , or ]
				}
			}
		}
	}

	if lastSafeEnd >= 0 {
		// Truncate at the last safe point.
		// If it's a comma, drop the trailing comma before closing.
		truncated := s[:lastSafeEnd+1]
		truncated = strings.TrimRight(truncated, " 	\n\r")
		truncated = strings.TrimSuffix(truncated, ",")
		// Close all unclosed brackets from the truncated point.
		stack := unclosedStack(truncated)
		for i := len(stack) - 1; i >= 0; i-- {
			switch stack[i] {
			case '{':
				truncated += "}"
			case '[':
				truncated += "]"
			}
		}

		if isValid(truncated) {
			return truncated, true
		}
	}

	// Fallback: use Repair.
	return Repair(s)
}

// --- internal helpers ---

func isValid(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

func closeAtEnd(s string) string {
	inString := false
	escaped := false
	stack := []byte{}

	for i := 0; i < len(s); i++ {
		c := s[i]

		if escaped {
			escaped = false
			continue
		}
		if inString {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}

		switch c {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}

	result := s

	// Close open string first.
	if inString {
		// If the last char is an unescaped backslash, add another backslash
		// so the closing quote isn't escaped.
		if escaped {
			result += `\`
		}
		result += `"`
	}

	// Strip trailing comma before closing brackets (e.g. {"a":1,"b":2, → {"a":1,"b":2)
	result = strings.TrimRight(result, " 	\n\r")
	result = strings.TrimSuffix(result, ",")

	// Close unclosed brackets in reverse order.
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i] {
		case '{':
			result += "}"
		case '[':
			result += "]"
		}
	}

	return result
}

// closeAtLastComplete finds the last position where a complete JSON value
// ends (followed by , } or ]) and closes brackets from there.
func closeAtLastComplete(s string) string {
	inString := false
	escaped := false
	lastComplete := -1

	for i := 0; i < len(s); i++ {
		c := s[i]

		if escaped {
			escaped = false
			continue
		}
		if inString {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
				// Check if a complete value follows.
				j := i + 1
				for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
					j++
				}
				if j < len(s) && (s[j] == ',' || s[j] == '}' || s[j] == ']') {
					lastComplete = j
				}
			}
			continue
		}

		switch c {
		case '"':
			inString = true
		}
	}

	if lastComplete < 0 {
		return closeAtEnd(s)
	}

	// Truncate at lastComplete, strip trailing comma, then close brackets.
	candidate := s[:lastComplete+1]
	candidate = strings.TrimRight(candidate, " \t\n\r")
	candidate = strings.TrimSuffix(candidate, ",")

	// Recompute unclosed bracket stack for the truncated text.
	stack := unclosedStack(candidate)
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i] {
		case '{':
			candidate += "}"
		case '[':
			candidate += "]"
		}
	}

	return candidate
}

// unclosedStack computes the stack of unclosed brackets in a JSON text.
func unclosedStack(s string) []byte {
	var stack []byte
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		c := s[i]

		if escaped {
			escaped = false
			continue
		}
		if inString {
			if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}

		switch c {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}

	return stack
}
