package core

import (
	"bytes"
	"encoding/json"
	"fmt"
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
		return jsonSkeleton(data)
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

func jsonSkeleton(data []byte) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[JSON Skeleton: %d KB, %d lines]\n", len(data)/1024, bytes.Count(data, []byte("\n"))+1))

	dec := json.NewDecoder(bytes.NewReader(data))
	depth := 0
	var ctx []byte // stack: 'o' = object, 'a' = array
	arrayCount := []int{}
	arrayStartLine := []int{}
	inKey := false  // next string token is a key
	skipDepth := -1 // skip array elements beyond first at this depth

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		offset := int(dec.InputOffset())
		line := byteToLine(data, offset)

		if skipDepth >= 0 && depth > skipDepth {
			// We're inside an array element we're skipping
			if d, ok := tok.(json.Delim); ok {
				switch d {
				case '{', '[':
					depth++
				case '}', ']':
					depth--
					if depth == skipDepth {
						skipDepth = -1
					}
				}
			}
			continue
		}

		switch t := tok.(type) {
		case json.Delim:
			inKey = false
			switch t {
			case '{':
				if depth == 0 {
					sb.WriteString(fmt.Sprintf("L%d: {\n", line))
				}
				depth++
				ctx = append(ctx, 'o')
				inKey = true
			case '}':
				depth--
				ctx = ctx[:len(ctx)-1]
				inKey = false
			case '[':
				depth++
				ctx = append(ctx, 'a')
				arrayCount = append(arrayCount, 0)
				arrayStartLine = append(arrayStartLine, line)
				skipDepth = depth // skip after first element
			case ']':
				count := arrayCount[len(arrayCount)-1]
				startLine := arrayStartLine[len(arrayStartLine)-1]
				indent := strings.Repeat("  ", depth-1)
				if count > 0 {
					sb.WriteString(fmt.Sprintf("L%d-L%d: %s[array: %d elements]\n", startLine, line, indent, count))
				}
				depth--
				ctx = ctx[:len(ctx)-1]
				arrayCount = arrayCount[:len(arrayCount)-1]
				arrayStartLine = arrayStartLine[:len(arrayStartLine)-1]
				if skipDepth == depth+1 {
					skipDepth = -1
				}
			}
		case string:
			if depth > 0 && len(ctx) > 0 && ctx[len(ctx)-1] == 'o' {
				if inKey {
					indent := strings.Repeat("  ", depth-1)
					sb.WriteString(fmt.Sprintf("L%d: %s\"%s\":", line, indent, t))
					inKey = false
				} else {
					sb.WriteString(" string\n")
					inKey = true
				}
			} else if depth > 0 && len(ctx) > 0 && ctx[len(ctx)-1] == 'a' {
				arrayCount[len(arrayCount)-1]++
				if arrayCount[len(arrayCount)-1] == 1 {
					indent := strings.Repeat("  ", depth-1)
					sb.WriteString(fmt.Sprintf("L%d: %s(first element)\n", line, indent))
				}
			}
		case float64:
			if depth > 0 && len(ctx) > 0 && ctx[len(ctx)-1] == 'o' && !inKey {
				sb.WriteString(" number\n")
				inKey = true
			}
		case bool:
			if depth > 0 && len(ctx) > 0 && ctx[len(ctx)-1] == 'o' && !inKey {
				sb.WriteString(" bool\n")
				inKey = true
			}
		case nil:
			if depth > 0 && len(ctx) > 0 && ctx[len(ctx)-1] == 'o' && !inKey {
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
