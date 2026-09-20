package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SummarizeOutput detects the format of data and returns a compact summary.
// Used by both read_file (mode=summary) and run_command (auto-trigger for large output).
// Falls back to AdaptiveOutputResult for unknown or small content.
func SummarizeOutput(data []byte, hint string) string {
	if len(data) < 8*1024 {
		return string(data)
	}

	format := detectFormat(data)
	switch format {
	case "json":
		return summarizeJSONOutput(data)
	case "jsonl":
		return summarizeJSONL(data)
	case "table":
		return summarizeTable(data)
	case "log":
		return summarizeLog(data)
	default:
		return string(AdaptiveOutputResult(data, hint))
	}
}

// detectFormat sniffs the first few lines to identify the content format.
func detectFormat(data []byte) string {
	s := string(data)
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if len(trimmed) == 0 {
		return "plain"
	}

	if trimmed[0] == '{' || trimmed[0] == '[' {
		var v interface{}
		if json.Unmarshal(data, &v) == nil {
			return "json"
		}
	}

	lines := strings.SplitN(s, "\n", 6)
	if len(lines) >= 2 {
		jsonLineCount := 0
		for i := 0; i < len(lines) && i < 5; i++ {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			var v interface{}
			if json.Unmarshal([]byte(line), &v) == nil {
				jsonLineCount++
			}
		}
		if jsonLineCount >= 2 {
			return "jsonl"
		}
	}

	if len(lines) >= 3 {
		pipeCount := 0
		for i := 0; i < len(lines) && i < 5; i++ {
			if strings.Count(lines[i], "|") >= 2 {
				pipeCount++
			}
		}
		if pipeCount >= 3 {
			return "table"
		}
	}

	logKeywords := []string{"ERROR", "WARN", "INFO", "DEBUG", "FATAL", "TRACE"}
	keywordHits := 0
	for i := 0; i < len(lines) && i < 10; i++ {
		upper := strings.ToUpper(lines[i])
		for _, kw := range logKeywords {
			if strings.Contains(upper, kw) {
				keywordHits++
				break
			}
		}
	}
	if keywordHits >= 3 {
		return "log"
	}

	return "plain"
}

func summarizeJSONOutput(data []byte) string {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return string(AdaptiveOutputResult(data, "json"))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[JSON Summary: %d KB]\n", len(data)/1024))
	walkJSONSummary(v, "", &sb)
	return sb.String()
}

func walkJSONSummary(v interface{}, prefix string, sb *strings.Builder) {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			child := t[k]
			switch c := child.(type) {
			case map[string]interface{}:
				sb.WriteString(fmt.Sprintf("%s%s: {object:%d keys}\n", prefix, k, len(c)))
				if len(prefix) < 6 {
					walkJSONSummary(c, prefix+"  ", sb)
				}
			case []interface{}:
				sb.WriteString(fmt.Sprintf("%s%s: [array:%d]\n", prefix, k, len(c)))
				if len(c) > 0 && len(prefix) < 6 {
					walkJSONSummary(c[0], prefix+"  ", sb)
				}
			default:
				sb.WriteString(fmt.Sprintf("%s%s: %s\n", prefix, k, typeLabel(child)))
			}
		}
	case []interface{}:
		sb.WriteString(fmt.Sprintf("%s[array:%d]\n", prefix, len(t)))
		if len(t) > 0 && len(prefix) < 6 {
			walkJSONSummary(t[0], prefix+"  ", sb)
		}
	}
}

func typeLabel(v interface{}) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return "?"
	}
}

func summarizeJSONL(data []byte) string {
	lines := strings.Split(string(data), "\n")
	total := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		total++
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[JSONL Summary: %d records, %d KB]\n", total, len(data)/1024))

	shown := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if shown < 3 || shown >= total-3 {
			var v interface{}
			if json.Unmarshal([]byte(line), &v) == nil {
				if m, ok := v.(map[string]interface{}); ok {
					keys := make([]string, 0, len(m))
					for k := range m {
						keys = append(keys, k)
					}
					sortStrings(keys)
					sb.WriteString(fmt.Sprintf("  record#%d: keys=%v\n", shown, keys))
				}
			}
		}
		shown++
		if shown == 3 && total > 6 {
			sb.WriteString(fmt.Sprintf("  ... [%d records omitted] ...\n", total-6))
		}
	}
	return sb.String()
}

func summarizeTable(data []byte) string {
	lines := strings.Split(string(data), "\n")
	nonEmpty := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty++
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Table Summary: %d rows, %d KB]\n", nonEmpty, len(data)/1024))

	shown := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if shown < 3 {
			sb.WriteString(fmt.Sprintf("  %s\n", strings.TrimSpace(line)))
		}
		shown++
		if shown == 3 {
			sb.WriteString(fmt.Sprintf("  ... [%d rows omitted] ...\n", nonEmpty-3))
			break
		}
	}
	return sb.String()
}

func summarizeLog(data []byte) string {
	lines := strings.Split(string(data), "\n")
	levelCounts := map[string]int{"ERROR": 0, "WARN": 0, "INFO": 0, "DEBUG": 0, "FATAL": 0, "TRACE": 0}

	var errorLines []string
	for _, line := range lines {
		upper := strings.ToUpper(line)
		for kw := range levelCounts {
			if strings.Contains(upper, kw) {
				levelCounts[kw]++
				if (kw == "ERROR" || kw == "FATAL") && len(errorLines) < 10 {
					errorLines = append(errorLines, strings.TrimSpace(line))
				}
				break
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Log Summary: %d lines, %d KB]\n", len(lines), len(data)/1024))
	sb.WriteString("  Level counts:\n")
	for _, kw := range []string{"FATAL", "ERROR", "WARN", "INFO", "DEBUG", "TRACE"} {
		if levelCounts[kw] > 0 {
			sb.WriteString(fmt.Sprintf("    %s: %d\n", kw, levelCounts[kw]))
		}
	}
	if len(errorLines) > 0 {
		sb.WriteString("  Error/Fatal lines:\n")
		for _, l := range errorLines {
			sb.WriteString(fmt.Sprintf("    %s\n", l))
		}
	}
	return sb.String()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
