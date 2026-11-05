package schemas

import "testing"

// Q2-4 (2026-11-06): only declarative exit_criteria are legal. This classifier
// is what the SOP loader uses to warn about legacy prose; prose is still
// substring-matched at runtime, so the pin here is about the authoring
// contract, not about changing what an existing SOP does.

func TestExitCriteriaIsDeclarative(t *testing.T) {
	legal := []string{
		"",
		"   ",
		"json",
		"regex:https?://.*\\.png",
		"file:workspace/tmp/report.md",
		"regex:  padded  ",
		"file:  padded.md  ",
	}
	for _, c := range legal {
		if !ExitCriteriaIsDeclarative(c) {
			t.Errorf("ExitCriteriaIsDeclarative(%q) = false, want true", c)
		}
	}

	illegal := []string{
		"SOP saved successfully and validateSOPDAG passes",
		"gate_passed=true",
		"regex:",
		"regex:   ",
		"file:",
		"file:   ",
		"JSON",
		"files:report.md",
	}
	for _, c := range illegal {
		if ExitCriteriaIsDeclarative(c) {
			t.Errorf("ExitCriteriaIsDeclarative(%q) = true, want false", c)
		}
	}
}
