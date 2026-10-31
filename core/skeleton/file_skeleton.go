package skeleton

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// FileSkeleton returns a structural skeleton of a file with line numbers.
// Detects format by file extension and applies the appropriate summarizer.
// Falls back to SummarizeOutput for unknown extensions.
func FileSkeleton(data []byte, filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".json":
		return safeJSONSkeleton(data)
	case ".html", ".htm", ".xml", ".svg":
		return tagSkeleton(data)
	case ".go":
		return codeSkeleton(data, goSymbols)
	case ".py":
		return codeSkeleton(data, pySymbols)
	case ".js", ".ts", ".jsx", ".tsx":
		return codeSkeleton(data, jsSymbols)
	case ".java", ".c", ".cpp", ".h", ".hpp", ".rs":
		return codeSkeleton(data, clikeSymbols)
	default:
		return SummarizeOutput(data, filename)
	}
}

// byteToLine converts a byte offset to a 1-indexed line number.
func byteToLine(data []byte, offset int) int {
	if offset > len(data) {
		offset = len(data)
	}
	if offset <= 0 {
		return 1
	}
	return bytes.Count(data[:offset], []byte("\n")) + 1
}

// --- JSON Skeleton ---

// jsonFrame is one open container in jsonSkeleton.
//
// It is deliberately a single value instead of three parallel slices
// (ctx []byte, arrayCount []int, arrayStartLine []int). The parallel-slice
// version desynchronised whenever an opener was consumed by the printing
// path and its matching closer by the "skip elements beyond the first"
// path, and the resulting "-1 index" panic killed a whole chat turn
// (see safeJSONSkeleton's doc comment for the observed impact). One frame
// per opener makes that class of bug unrepresentable.
type jsonFrame struct {
	kind      byte // '{' or '['
	count     int  // array elements seen (arrays only)
	startLine int  // line of the opening bracket (arrays only)
}

// closerOf returns the delimiter that closes kind.
func closerOf(kind byte) byte {
	if kind == '{' {
		return '}'
	}
	return ']'
}

// safeJSONSkeleton is jsonSkeleton with a panic containment boundary.
//
// The skeletonizer exists to SHORTEN a tool result; it is an optimisation,
// never a correctness boundary, and it parses arbitrary model/tool output.
// A bug in it must therefore degrade to the plain summariser, not take down
// the caller. This is not hypothetical: on 2026-10-30 a 5804-byte, fully
// valid JSON read_file result panicked here (index out of range [-1]), the
// panic was recovered upstream and only logged, and because a recovered
// panic emits no terminal SSE event the chat stream never closed — the turn
// idled for 1137 s after the model had finished at t=22 s and then scored
// model_error. Containment here + a terminal event upstream means the worst
// case is a slightly uglier skeleton.
func safeJSONSkeleton(data []byte) (out string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[skeleton] WARN: jsonSkeleton panicked (%v); falling back to flat summary\n", r)
			out = SummarizeOutput(data, "json")
		}
	}()
	return jsonSkeleton(data)
}

func jsonSkeleton(data []byte) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[JSON Skeleton: %d KB, %d lines]\n", len(data)/1024, bytes.Count(data, []byte("\n"))+1))

	dec := json.NewDecoder(bytes.NewReader(data))
	depth := 0
	stack := []jsonFrame{}
	inKey := false  // next string token is a key
	skipDepth := -1 // suppress output for containers deeper than this

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		offset := int(dec.InputOffset())
		line := byteToLine(data, offset)

		// skipping suppresses OUTPUT only. The container stack is maintained
		// identically on both paths, so depth and stack can never disagree.
		skipping := skipDepth >= 0 && depth > skipDepth

		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				// Record the array's own depth so the "first element" summary
				// can print, then suppress anything deeper than one level.
				if !skipping && d == '[' {
					skipDepth = depth + 1
				}
				if !skipping && d == '{' && depth == 0 {
					sb.WriteString(fmt.Sprintf("L%d: {\n", line))
				}
				depth++
				stack = append(stack, jsonFrame{kind: byte(d), startLine: line})
				if !skipping {
					inKey = d == '{'
				}
			case '}', ']':
				// audit fix F: only pop the frame this delimiter actually
				// closes. A stray or mismatched closer in non-JSON tool output
				// must be ignored, not silently corrupt the stack.
				n := len(stack)
				if n == 0 || closerOf(stack[n-1].kind) != byte(d) {
					continue
				}
				f := stack[n-1]
				stack = stack[:n-1]
				if !skipping && d == ']' && f.count > 0 {
					indent := strings.Repeat("  ", depth-1)
					sb.WriteString(fmt.Sprintf("L%d-L%d: %s[array: %d elements]\n", f.startLine, line, indent, f.count))
				}
				depth--
				if skipDepth >= 0 && depth <= skipDepth {
					skipDepth = -1
				}
				if !skipping {
					inKey = false
				}
			}
			continue
		}

		if skipping || depth == 0 || len(stack) == 0 {
			continue
		}
		top := &stack[len(stack)-1]
		indent := strings.Repeat("  ", depth-1)

		switch t := tok.(type) {
		case string:
			if top.kind == '{' {
				if inKey {
					sb.WriteString(fmt.Sprintf("L%d: %s\"%s\":", line, indent, t))
					inKey = false
				} else {
					sb.WriteString(" string\n")
					inKey = true
				}
			} else {
				top.count++
				if top.count == 1 {
					sb.WriteString(fmt.Sprintf("L%d: %s(first element)\n", line, indent))
				}
			}
		case float64:
			if top.kind == '{' && !inKey {
				sb.WriteString(" number\n")
				inKey = true
			}
		case bool:
			if top.kind == '{' && !inKey {
				sb.WriteString(" bool\n")
				inKey = true
			}
		case nil:
			if top.kind == '{' && !inKey {
				sb.WriteString(" null\n")
				inKey = true
			}
		}
	}
	return sb.String()
}

// --- HTML/XML Tag Skeleton ---

var tagRe = regexp.MustCompile(`(?i)</?([a-zA-Z][a-zA-Z0-9]*)[^>]*?(/?)>`)

func tagSkeleton(data []byte) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Tag Skeleton: %d KB, %d lines]\n", len(data)/1024, bytes.Count(data, []byte("\n"))+1))

	depth := 0
	matches := tagRe.FindAllIndex(data, -1)
	for _, m := range matches {
		line := byteToLine(data, m[0])
		tagText := string(data[m[0]:m[1]])
		isClose := strings.HasPrefix(tagText, "</")
		isSelfClose := strings.HasSuffix(tagText, "/>")
		sub := tagRe.FindSubmatch(data[m[0]:m[1]])
		tagName := ""
		if len(sub) >= 2 {
			tagName = string(sub[1])
		}

		if isClose {
			depth--
			if depth < 0 {
				depth = 0
			}
		}

		indent := strings.Repeat("  ", depth)
		if isClose {
			sb.WriteString(fmt.Sprintf("L%d: %s</%s>\n", line, indent, tagName))
		} else if isSelfClose {
			sb.WriteString(fmt.Sprintf("L%d: %s<%s/>\n", line, indent, tagName))
		} else {
			sb.WriteString(fmt.Sprintf("L%d: %s<%s>\n", line, indent, tagName))
			depth++
		}

		if sb.Len() > 4000 {
			sb.WriteString(fmt.Sprintf("  ... [%d more tags omitted] ...\n", len(matches)-len(sb.String())))
			break
		}
	}
	return sb.String()
}

// --- Code Symbol Skeleton ---

type symbolPattern struct {
	re   *regexp.Regexp
	kind string
}

var goSymbols = []symbolPattern{
	{regexp.MustCompile(`^func\s+(\([^)]+\)\s+)?([A-Za-z_][A-Za-z0-9_]*)`), "func"},
	{regexp.MustCompile(`^type\s+([A-Za-z_][A-Za-z0-9_]*)\s+(?:struct|interface)`), "type"},
	{regexp.MustCompile(`^var\s+([A-Za-z_][A-Za-z0-9_]*)`), "var"},
	{regexp.MustCompile(`^const\s+([A-Za-z_][A-Za-z0-9_]*)`), "const"},
}

var pySymbols = []symbolPattern{
	{regexp.MustCompile(`^(async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)`), "def"},
	{regexp.MustCompile(`^class\s+([A-Za-z_][A-Za-z0-9_]*)`), "class"},
}

var jsSymbols = []symbolPattern{
	{regexp.MustCompile(`^(export\s+)?(async\s+)?function\s+([A-Za-z_$][A-Za-z0-9_$]*)`), "function"},
	{regexp.MustCompile(`^(export\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`), "class"},
	{regexp.MustCompile(`^(export\s+)?const\s+([A-Za-z_$][A-Za-z0-9_$]*)`), "const"},
	{regexp.MustCompile(`^(export\s+)?interface\s+([A-Za-z_$][A-Za-z0-9_$]*)`), "interface"},
}

var clikeSymbols = []symbolPattern{
	{regexp.MustCompile(`^\s*(public|private|protected|static)?\s*(class|struct|void|int|float|double|bool|string|auto|fn|pub\s+fn)\s+([A-Za-z_][A-Za-z0-9_]*)`), "symbol"},
}

func codeSkeleton(data []byte, patterns []symbolPattern) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Code Skeleton: %d KB, %d lines]\n", len(data)/1024, bytes.Count(data, []byte("\n"))+1))

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, p := range patterns {
			if m := p.re.FindStringSubmatch(trimmed); m != nil {
				name := m[len(m)-1]
				sb.WriteString(fmt.Sprintf("L%d: %s %s\n", i+1, p.kind, name))
				break
			}
		}
		if sb.Len() > 4000 {
			sb.WriteString(fmt.Sprintf("  ... [remaining lines omitted] ...\n"))
			break
		}
	}
	return sb.String()
}
