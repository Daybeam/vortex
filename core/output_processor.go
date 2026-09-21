package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// OutputProcessor handles the transformation and summarization of tool results.
type OutputProcessor struct {
	MaxBodyChars int
}

func NewOutputProcessor() *OutputProcessor {
	return &OutputProcessor{
		MaxBodyChars: 3000, // Roughly 1000 tokens
	}
}

// Wrap converts a raw tool result into a structured SemanticEnvelope.
func (p *OutputProcessor) Wrap(toolName string, raw any, status string, source string, reason string) *schemas.SemanticEnvelope {
	env := &schemas.SemanticEnvelope{
		Status:    status,
		Source:    source,
		Timestamp: time.Now().Unix(),
		Reason:    reason,
	}

	// Determine type and content
	switch v := raw.(type) {
	case string:
		env.Type = "text"
		env.Content = p.truncate(v)
	case []byte:
		env.Type = "text"
		env.Content = p.truncate(string(v))
	default:
		env.Type = "json"
		data, _ := json.MarshalIndent(v, "", "  ")
		env.Content = p.truncate(string(data))
	}

	// Extract citations (e.g., file paths, IDs)
	env.Citations = p.extractCitations(env.Content)

	return env
}

func (p *OutputProcessor) truncate(s string) string {
	if len(s) <= p.MaxBodyChars {
		return s
	}

	// If it's a very large output, try to keep the head and tail
	head := s[:p.MaxBodyChars/2]
	tail := s[len(s)-p.MaxBodyChars/2:]

	return fmt.Sprintf("%s\n\n... [TRUNCATED %d characters - Content side-loaded to outputs/ directory] ...\n\n%s", head, len(s)-p.MaxBodyChars, tail)
}

func (p *OutputProcessor) extractCitations(s string) []string {
	// Simple heuristic: look for things that look like paths or IDs
	var citations []string

	// We'll just do a very basic check for now to avoid too much noise
	lines := strings.Split(s, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Error:") || strings.Contains(line, "FAILED") {
			citations = append(citations, strings.TrimSpace(line))
		}
	}

	if len(citations) > 3 {
		return citations[:3]
	}
	return citations
}
