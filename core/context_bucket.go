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
// See docs/completed/2026-10-30/VCP_HEARTBEAT_AND_CONTEXT_TIERING_DESIGN.md §3 Bucket 2.
const MaxFullObservations = 5

// summarizedInfix separates an already-compacted observation from a fresh one.
// The producer always emits MarkerEnvObservation followed by "\n", so this infix
// can only come from a summary this function wrote.
const summarizedInfix = " (summarized, "

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
//	"[ENVIRONMENT OBSERVATION] (summarized, XKB) <first line of original>"
//
// This is a pure function: it returns a new slice, never mutates the input.
// It is also idempotent — an already-summarized block is left untouched, so
// running it once per turn does not re-summarize the previous turns' summaries.
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
		// Idempotent: the caller runs this every turn on a slice that still
		// carries earlier summaries. Re-compacting one would measure the
		// summary's own length and nest the marker, losing the original size
		// and first line -- the only two facts worth keeping.
		if strings.HasPrefix(original, MarkerEnvObservation+summarizedInfix) {
			continue
		}
		kb := len(original) / 1024
		if kb == 0 {
			kb = 1 // "0KB" is misleading for non-empty content
		}
		firstLine := extractFirstLine(original)
		result[idx] = schemas.ContentBlock{
			Text: fmt.Sprintf("%s%s%dKB) %s", MarkerEnvObservation, summarizedInfix, kb, firstLine),
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
