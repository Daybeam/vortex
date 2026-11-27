package core

import (
	"hash/fnv"
	"strings"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Echo guard — detects when the model emits verbatim identical prose across
// consecutive turns with no tool calls. This fills the gap left by B2 (which
// requires len(toolResults) > 0) and the spin breaker (which requires parse
// failures). The cross product {valid, parseable} × {identical} × {no tools}
// was previously unguarded.
//
// Design: reuse B2's proven shape (consecutive-only FNV hash, hint-then-kill,
// grace window) but on the model's text with fenced-block stripping so
// diagrams/code the task asked for are never counted.

// MinEchoBytes is the minimum prose length (after fence stripping) to count.
// Short confirmations ("OK", "Done") never trigger. Matches B2's floor.
const MinEchoBytes = 100

// MaxEchoRepeats is the hint threshold — mirrors B2's maxConsecutiveDuplicates.
const MaxEchoRepeats = 3

// EchoAction is the result of checking one turn's text against the echo guard.
type EchoAction int

const (
	EchoNone EchoAction = iota // no repetition detected
	EchoHint                   // hint should be injected
	EchoKill                   // kill: return partial output
)

// EchoGuard holds loop-local state for verbatim prose echo detection.
// Create one per turn loop (per doSpawn or per ChatHarness.Run), call Check
// on each turn's text. Zero-value is ready to use with grace=0.
type EchoGuard struct {
	lastHash    uint64
	repeatCount int
	hinted      bool
	hintAt      int
	graceTurns  int
}

// NewEchoGuard creates an echo guard with the given grace window.
// grace=0 means hint at MaxEchoRepeats, kill on the very next repeat
// (historical B2 behaviour).
func NewEchoGuard(graceTurns int) *EchoGuard {
	return &EchoGuard{graceTurns: graceTurns}
}

// Check examines one turn's text and returns the action to take.
// On EchoHint, the caller should inject EchoHintText and continue the loop.
// On EchoKill, the caller should return a partial result.
// On EchoNone, the turn is clean — proceed normally.
func (g *EchoGuard) Check(text string) EchoAction {
	hash, shouldTrack := computeEchoHash(text)
	if !shouldTrack {
		g.lastHash = 0
		g.repeatCount = 0
		return EchoNone
	}
	if hash == g.lastHash {
		g.repeatCount++
	} else {
		g.lastHash = hash
		g.repeatCount = 1
	}
	switch stagnationStep(g.repeatCount, g.hintAt, g.graceTurns, MaxEchoRepeats, g.hinted) {
	case stagHint:
		g.hinted = true
		g.hintAt = g.repeatCount
		return EchoHint
	case stagKill:
		return EchoKill
	default:
		return EchoNone
	}
}

// stripFencedBlocks removes all ```...``` spans from text, leaving only the
// prose outside fences. This is the key trick: legitimate re-emission of
// diagrams/code lives inside fences; pathological prose echo lives outside.
// Fence-only turns (diagram, no prose) produce empty output → skipped entirely.
func stripFencedBlocks(text string) string {
	var b strings.Builder
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// computeEchoHash returns the FNV-64a hash of the fence-stripped prose, and
// whether the prose is long enough to track (>= MinEchoBytes). Short turns
// and fence-only turns are skipped — they legitimately repeat.
func computeEchoHash(text string) (hash uint64, shouldTrack bool) {
	echoText := stripFencedBlocks(text)
	if len(echoText) < MinEchoBytes {
		return 0, false
	}
	h := fnv.New64a()
	h.Write([]byte(echoText))
	return h.Sum64(), true
}

// EchoHintText is the nudge injected when the echo guard fires a hint.
const EchoHintText = "System: You have produced the identical response twice in a row. Either call a tool or produce a different answer."

// partialEchoOutput builds a SpawnResult with StatusPartial that preserves
// accumulated work when the model is stuck repeating identical prose. Clone of
// partialStagnationOutput with an echo-specific reason string.
func (s *Spawner) partialEchoOutput(capability, reason string, trace []store.ToolInteraction) *SpawnResult {
	toolNames := make([]string, 0, len(trace))
	for _, t := range trace {
		toolNames = append(toolNames, t.ToolName)
	}
	return &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusPartial,
			Confidence: 0.3,
			Result: map[string]any{
				"error":           reason,
				"tools_used":      toolNames,
				"tool_call_count": len(trace),
			},
			Assumptions: []string{"echo_detected: " + reason},
			Warnings:    []string{reason},
			Capability:  capability,
		},
	}
}
