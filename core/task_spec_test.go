package core

import (
	"strings"
	"testing"
)

// The real 011 prompt shape: instruction body, "---", then the contract.
const samplePrompt = "Prepare the scheduling handoff for request `CAL-NW-482`\n" +
	"using only `/workspace/meeting_request.json`.\n" +
	"\n" +
	"Create `/workspace/scheduling_plan.json` containing:\n" +
	"\n" +
	"- `request_id`, `status`, and `timezone`;\n" +
	"- a `participant_local_times` object for the recommended candidate, keyed by\n" +
	"  participant email, with a human-readable local date and start/end time;\n" +
	"\n" +
	"Do not modify the request or calendar snapshots and do not use the network.\n" +
	"\n" +
	"---\n" +
	"\n" +
	"## Enforced workspace change contract\n" +
	"\n" +
	"The controller compares the final state of `/workspace` after every round.\n" +
	"\n" +
	"- Create: `customer_reply.md`, `scheduling_plan.json`\n" +
	"- Modify: none\n" +
	"- Delete: none\n"

func TestSpecSectionsSplitsInstructionFromContract(t *testing.T) {
	instruction, contract := SpecSections(samplePrompt)

	if want := "and do not use the network."; !strings.Contains(instruction, want) {
		t.Fatalf("instruction lost its last line %q:\n%s", want, instruction)
	}
	if strings.Contains(instruction, WorkspaceContractHeader) {
		t.Fatalf("instruction must not contain the contract heading:\n%s", instruction)
	}
	if strings.Contains(instruction, "\n---") {
		t.Fatalf("trailing separator should be trimmed, got:\n%q", instruction)
	}
	// The clauses the v73 parents dropped. If the splitter loses either, the
	// append cannot restore it (iron law 178).
	for _, want := range []string{"start/end time", "keyed by"} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("instruction lost %q", want)
		}
	}
	for _, want := range []string{"Delete: none", "Modify: none", "Create: `customer_reply.md`"} {
		if !strings.Contains(contract, want) {
			t.Fatalf("contract lost %q:\n%s", want, contract)
		}
	}
	if !strings.HasPrefix(contract, WorkspaceContractHeader) {
		t.Fatalf("contract must start at the heading, got:\n%.60q", contract)
	}
}

func TestSpecSectionsWithoutContract(t *testing.T) {
	prompt := "Do the thing.\n\nBe careful.\n"
	instruction, contract := SpecSections(prompt)
	if contract != "" {
		t.Fatalf("no heading must mean no contract, got %q", contract)
	}
	if instruction != "Do the thing.\n\nBe careful." {
		t.Fatalf("instruction should be the trimmed whole prompt, got %q", instruction)
	}
}

func TestSpecSectionsStopsAtNextHeading(t *testing.T) {
	prompt := "Body.\n\n## Enforced workspace change contract\n\n- Delete: none\n\n## Appendix\n\nnot the contract\n"
	_, contract := SpecSections(prompt)
	if strings.Contains(contract, "Appendix") || strings.Contains(contract, "not the contract") {
		t.Fatalf("contract ran past the next heading:\n%s", contract)
	}
	if !strings.Contains(contract, "Delete: none") {
		t.Fatalf("contract lost its body:\n%s", contract)
	}
}

func TestSpecSectionsEmpty(t *testing.T) {
	for _, in := range []string{"", "   \n\t\n"} {
		instruction, contract := SpecSections(in)
		if instruction != "" || contract != "" {
			t.Fatalf("empty prompt must yield empty sections, got %q / %q", instruction, contract)
		}
	}
}

func TestSpecSectionsHeaderCaseAndIndent(t *testing.T) {
	prompt := "Body.\n\n  ## enforced WORKSPACE change contract  \n\n- Delete: none\n"
	_, contract := SpecSections(prompt)
	if !strings.Contains(contract, "Delete: none") {
		t.Fatalf("case/indent-insensitive header not matched:\n%q", contract)
	}
}
