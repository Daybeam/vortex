package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

// MaxFullObservations is the default number of recent tool-observation
// blocks kept in full. Older observations are compacted to one-line
// summaries to prevent context pollution in long-running heartbeat tasks.
//
// See docs/plan/VCP_HEARTBEAT_AND_CONTEXT_TIERING_DESIGN.md §3 Bucket 2.
const MaxFullObservations = 5

// CompactOldObservations implements the Rolling Log Bucket (Bucket 2 of
// the VCP Context Tiering design). It keeps the latest maxFull observation
// blocks in full and replaces older ones with compact one-line summaries.
//
// Observation blocks are identified by the MarkerEnvObservation prefix
// ("[ENVIRONMENT OBSERVATION]"). Assistant-turn blocks (no marker) are
// always preserved — the model needs to see what it said, even if the
// tool result is summarized.
//
// The summary format is:
//
//	"[ENVIRONMENT OBSERVATION (summarized, XKB)] <first line of original>"
//
// This is a pure function: it returns a new slice, never mutates the input.
func CompactOldObservations(userBlocks []schemas.ContentBlock, maxFull int) []schemas.ContentBlock {
	if maxFull <= 0 {
		maxFull = MaxFullObservations
	}

	// Count observation blocks.
	obsIndices := make([]int, 0, len(userBlocks))
	for i, b := range userBlocks {
		if strings.HasPrefix(b.Text, MarkerEnvObservation) {
			obsIndices = append(obsIndices, i)
		}
	}

	// If within budget, no compaction needed.
	if len(obsIndices) <= maxFull {
		return userBlocks
	}

	// Create a copy; replace old observation blocks with summaries.
	result := make([]schemas.ContentBlock, len(userBlocks))
	copy(result, userBlocks)

	// Summarize the oldest observations (all except the last maxFull).
	toCompact := len(obsIndices) - maxFull
	for _, idx := range obsIndices[:toCompact] {
		original := result[idx].Text
		kb := len(original) / 1024
		if kb == 0 {
			kb = 1 // "0KB" is misleading for non-empty content
		}
		firstLine := extractFirstLine(original)
		result[idx] = schemas.ContentBlock{
			Text: fmt.Sprintf("%s (summarized, %dKB) %s", MarkerEnvObservation, kb, firstLine),
			// No CacheControl — summarized blocks should not be cached.
		}
	}

	return result
}

// extractFirstLine returns the first meaningful line of a tool observation
// block, skipping the marker prefix. Used to generate compact summaries.
func extractFirstLine(text string) string {
	// Strip the marker prefix.
	rest := strings.TrimPrefix(text, MarkerEnvObservation)
	rest = strings.TrimLeft(rest, "\n ")

	// Take the first line.
	idx := strings.IndexByte(rest, '\n')
	if idx > 0 {
		return rest[:idx]
	}
	// Single line — truncate if too long.
	if len(rest) > 120 {
		return rest[:117] + "..."
	}
	return rest
}
