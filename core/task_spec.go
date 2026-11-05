package core

import "strings"

// WorkspaceContractHeader is the heading harness-bench appends to every task
// prompt. It is the only part of the prompt that states what the controller
// will compare after the round — including the "Delete: none" line.
const WorkspaceContractHeader = "## Enforced workspace change contract"

// SpecSections splits a task prompt into its instruction body and its
// workspace-contract section, both verbatim.
//
// Why a splitter and not a parser: the two sections are consumed by text, not
// by logic. The delegation path has to hand the sub-agent the same words the
// parent was given, because the parent's own summary of them is lossy — in the
// v73 batch the parent turned "Do not leave any other created, modified, or
// deleted path" into "Ensure no extra files remain in /workspace", and the
// sub-agent then deleted the inputs (iron law 178). Reconstructing the text
// from parsed fields would reintroduce the same loss: the parsed form keeps
// "Create: a, b" but not the "human-readable local date and start/end time"
// clause that lives in the instruction body.
//
// contract is "" when the prompt has no such heading. instruction is then the
// whole prompt, which keeps callers uniform: appending an empty section is a
// no-op.
func SpecSections(prompt string) (instruction, contract string) {
	if strings.TrimSpace(prompt) == "" {
		return "", ""
	}
	lines := strings.Split(prompt, "\n")
	start := -1
	for i, line := range lines {
		if strings.EqualFold(strings.TrimSpace(line), WorkspaceContractHeader) {
			start = i
			break
		}
	}
	if start < 0 {
		return strings.TrimRight(prompt, " \t\n"), ""
	}
	// The contract runs to the next top-level heading, if any. Without this
	// bound a prompt that puts anything after the contract would have that
	// text copied into the "verbatim contract" section.
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	return trimInstructionTail(strings.Join(lines[:start], "\n")),
		strings.TrimRight(strings.Join(lines[start:end], "\n"), " \t\n")
}

// trimInstructionTail drops the separator and blank lines that precede the
// contract heading, so the appended block does not end with a dangling "---".
func trimInstructionTail(s string) string {
	lines := strings.Split(s, "\n")
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last != "" && last != "---" {
			break
		}
		lines = lines[:len(lines)-1]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t")
}
