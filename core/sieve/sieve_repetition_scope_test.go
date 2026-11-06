package sieve

import (
	"strings"
	"testing"
)

// This file pins the O16/O21 false-positive class in the text-repetition rule.
// Both incidents are 实测 and both were the engine destroying legitimate work:
//   - O16 (task_f31f1868): the mounted-skill index is joined with "\n---\n"
//     (core/skill_mounter.go:173), so two one-line descriptions render as two
//     bare "---" lines two apart, and the delegation hand-over was vetoed as
//     "output entered a logic loop" four times.
//   - O21 (task_e3e78311): the hunter step's own "findings.json — ONLY this
//     JSON" template opens nested objects on separate lines, which is the same
//     trip one rendering later. Replaying the rule over the captured payload
//     (tmp/delegation-sop-run/hunter_partial_output.txt, 163 lines) puts the
//     first hit on two bare "{" lines, with 2 offending pairs in the whole file.
//
// The rule before this change returned true on the *first* similar pair, and
// calculateSimilarity short-circuits a==b to 1.0, so one repeated punctuation
// line vetoed unconditionally — with no config key to widen it, because
// NewSieveFromConfig overrides only ToolRepetitionThreshold.

// hunterOutputExcerpt is the verbatim output-format block that vetoed
// task_e3e78311: nested JSON openers, two bare "{" lines within the 5-line
// window, plus the closers that made the second pair.
const hunterOutputExcerpt = `## Output format (findings.json — ONLY this JSON, no prose)
{
  "findings": [
    {
      "fingerprint": "<unique-stable-id>",
      "verdict": "confirmed|needs_validation|rejected",
      "title": "<short title>",
      "source_trace": [{"file": "<path>", "line": <N>, "symbol": "<func>", "evidence": "<code snippet>"}]
    }
  ]
}
`

// mountedSkillIndex is the O16 rendering: core/skill_mounter.go:173 joins the
// fragments with "\n---\n" and one-line descriptions leave two bare "---".
const mountedSkillIndex = `## Active Skills

### hunt
---
Auto-generated base skill for role_e57866a60dac workspace:map[…]
---
one-line description
`

func TestDetectRepetition_StructuralLinesAreNotALoop(t *testing.T) {
	s := NewSieve(10)
	for name, text := range map[string]string{
		"hunter output template": hunterOutputExcerpt,
		"mounted skill index":    mountedSkillIndex,
	} {
		if s.detectRepetition(text) {
			t.Errorf("%s: reported as a logic loop — a JSON/markdown delimiter line is a property of the format, not of the model", name)
		}
	}
}

// TestDetectRepetition_SingleProsePairIsNotALoop pins the second half of the
// rule: a loop needs two similar pairs, because one repeated line is what
// templates, tables and bullets look like.
func TestDetectRepetition_SingleProsePairIsNotALoop(t *testing.T) {
	s := NewSieve(10)
	if s.detectRepetition("intro line\n\n- Require a code-level boundary failure\n\ndetail line\n\n- Require a code-level boundary failure\n") {
		t.Error("one repeated prose line vetoed the output; minRepetitionPairs is 2 for exactly this shape")
	}
}

// A stuck model is still caught. This is the half that must not regress: the
// captured real loop in O21 scored 85 offending pairs, so the exemption is not a
// threshold anyone can outrun by emitting two lines.
func TestDetectRepetition_StuckLoopStillDetected(t *testing.T) {
	s := NewSieve(10)
	loop := strings.Repeat("I will now read the file and report the findings.\n", 20)
	if !s.detectRepetition(loop) {
		t.Error("20 identical prose lines no longer count as a logic loop")
	}

	// The pre-existing behaviour contract (docs/gherkin/BEHAVIOR_CONTRACTS.md
	// §Feature: Sieve Guardian) stays satisfied: interleaved repetition with a
	// 2-line window still trips.
	s2 := NewSieve(10)
	s2.WindowSize = 2
	s2.RepetitionThreshold = 0.9
	if !s2.detectRepetition("line 1\nline 2\nline 1\nline 2") {
		t.Error("the gherkin repetition contract regressed")
	}
}
