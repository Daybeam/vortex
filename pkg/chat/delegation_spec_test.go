package chat

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/core"
)

const specTaskPrompt = "Prepare the scheduling handoff for request `CAL-NW-482`.\n" +
	"\n" +
	"Create `/workspace/scheduling_plan.json` containing:\n" +
	"\n" +
	"- a `participant_local_times` object for the recommended candidate, keyed by\n" +
	"  participant email, with a human-readable local date and start/end time;\n" +
	"\n" +
	"Do not modify the request or calendar snapshots.\n" +
	"\n" +
	"---\n" +
	"\n" +
	"## Enforced workspace change contract\n" +
	"\n" +
	"- Create: `customer_reply.md`, `scheduling_plan.json`\n" +
	"- Modify: none\n" +
	"- Delete: none\n"

// The v73 r2 prompt, verbatim: the parent's own summary, which had already
// turned the delete ban into a cleanup order.
const r2ParentSummary = "Analyze `/workspace/meeting_request.json` and calendars.\n\n" +
	"Outputs:\n1. `/workspace/scheduling_plan.json`\n2. `/workspace/customer_reply.md`\n\n" +
	"Ensure no extra files remain in `/workspace`.\n"

func newSpecHarness(taskPrompt string) *ChatHarness {
	return &ChatHarness{taskPrompt: taskPrompt}
}

func TestDelegatedPromptCarriesVerbatimSpec(t *testing.T) {
	t.Setenv(DelegationSpecEnv, "on")
	h := newSpecHarness(specTaskPrompt)

	got := h.delegatedPromptWithSpec(r2ParentSummary)

	if !strings.HasPrefix(got, strings.TrimRight(r2ParentSummary, " \t\n")) {
		t.Fatalf("the parent's own prompt must survive as the prefix:\n%s", got)
	}
	// The two clauses the parents dropped in 5/5 and 4/5 of the v73 batch.
	for _, want := range []string{"Delete: none", "start/end time", "keyed by"} {
		if !strings.Contains(got, want) {
			t.Fatalf("appended prompt is missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, DelegationSpecPreamble) {
		t.Fatalf("missing the framing paragraph:\n%s", got)
	}
	if strings.Count(got, "## Enforced workspace change contract") != 1 {
		t.Fatalf("contract heading must appear once:\n%s", got)
	}
}

func TestDelegatedPromptUnchangedWhenOff(t *testing.T) {
	t.Setenv(DelegationSpecEnv, "off")
	h := newSpecHarness(specTaskPrompt)
	if got := h.delegatedPromptWithSpec(r2ParentSummary); got != r2ParentSummary {
		t.Fatalf("off must be byte-identical to the input, got:\n%s", got)
	}
}

func TestDelegatedPromptUnchangedWithoutTaskPrompt(t *testing.T) {
	t.Setenv(DelegationSpecEnv, "on")
	h := newSpecHarness("")
	if got := h.delegatedPromptWithSpec(r2ParentSummary); got != r2ParentSummary {
		t.Fatalf("no captured prompt must leave the input untouched, got:\n%s", got)
	}
}

func TestDelegatedPromptDoesNotDuplicateContract(t *testing.T) {
	t.Setenv(DelegationSpecEnv, "on")
	h := newSpecHarness(specTaskPrompt)
	// A parent that already pasted the contract keeps its own copy, untouched.
	// Quoting the contract says nothing about the task body, though: it is the
	// short tail that mentions `Delete: none`, and a parent can quote it while
	// still summarising the instruction from memory. Dropping the instruction
	// here would be silent, so only the contract is skipped.
	parent := r2ParentSummary + "\n" + coreContractForTest()
	got := h.delegatedPromptWithSpec(parent)

	if strings.Count(got, "## Enforced workspace change contract") != 1 {
		t.Fatalf("contract heading must appear once, not be duplicated:\n%s", got)
	}
	if !strings.Contains(got, "start/end time") {
		t.Fatalf("quoting the contract must not drop the instruction body:\n%s", got)
	}
}

// A parent that pasted the whole specification leaves nothing to add, so the
// append has to be a byte-for-byte no-op rather than a duplicate copy.
func TestDelegatedPromptUnchangedWhenSpecAlreadyPresent(t *testing.T) {
	t.Setenv(DelegationSpecEnv, "on")
	h := newSpecHarness(specTaskPrompt)
	parent := r2ParentSummary + "\n\n" + specTaskPrompt
	if got := h.delegatedPromptWithSpec(parent); got != parent {
		t.Fatalf("a prompt that already carries the spec must not be extended, got:\n%s", got)
	}
}

// The mirror case: a parent that pasted the instruction still needs the
// contract, because "Delete: none" lives only in the contract section.
func TestDelegatedPromptAppendsContractWhenOnlyInstructionPasted(t *testing.T) {
	t.Setenv(DelegationSpecEnv, "on")
	h := newSpecHarness(specTaskPrompt)
	instruction, _ := core.SpecSections(specTaskPrompt)
	parent := r2ParentSummary + "\n\n" + instruction

	got := h.delegatedPromptWithSpec(parent)

	if strings.Count(got, strings.TrimSpace(instruction)) != 1 {
		t.Fatalf("the pasted instruction must not be repeated:\n%s", got)
	}
	if !strings.Contains(got, "Delete: none") {
		t.Fatalf("the contract is missing from the parent and must be appended:\n%s", got)
	}
}

func TestDelegatedPromptAppendsInstructionWithoutContract(t *testing.T) {
	t.Setenv(DelegationSpecEnv, "on")
	h := newSpecHarness("Just do the thing.\n")
	got := h.delegatedPromptWithSpec("summary")
	if !strings.Contains(got, "Just do the thing.") {
		t.Fatalf("instruction body should still be appended:\n%s", got)
	}
	if strings.Contains(got, "### Verbatim — workspace change contract") {
		t.Fatalf("must not invent an empty contract section:\n%s", got)
	}
}

func coreContractForTest() string {
	return "## Enforced workspace change contract\n\n- Delete: none\n"
}

// ── Size guard ────────────────────────────────────────────────────────
// The append must never turn a delegable task into an unloadable prompt. A
// chat message can be a pasted document; this session already carries it, the
// sub-agent may not have room for it twice.

func bigBody(marker string, bytes int) string {
	line := strings.Repeat("x", 79) + "\n"
	return marker + "\n" + strings.Repeat(line, bytes/len(line)+1)
}

func TestDelegatedPromptDropsOversizeBody(t *testing.T) {
	t.Setenv(DelegationSpecEnv, "on")
	spec := bigBody("BODYMARKER-UNIQUE", DelegationSpecMaxBytes+1024) + "\n---\n\n" + coreContractForTest()
	h := newSpecHarness(spec)

	got := h.delegatedPromptWithSpec("summary")

	if strings.Contains(got, "BODYMARKER-UNIQUE") {
		t.Fatalf("an oversize body must not be appended:/n%.200s", got)
	}
	if !strings.Contains(got, "Delete: none") {
		t.Fatalf("the contract is small and must survive the cap:/n%s", got)
	}
	if !strings.Contains(got, "too large to re-append") {
		t.Fatalf("the omission must be stated, not silent:/n%s", got)
	}
}

func TestDelegatedPromptKeepsBodyAtCap(t *testing.T) {
	t.Setenv(DelegationSpecEnv, "on")
	body := strings.Repeat("y", DelegationSpecMaxBytes)
	h := newSpecHarness(body)

	got := h.delegatedPromptWithSpec("summary")

	if !strings.Contains(got, "### Verbatim — task specification") {
		t.Fatalf("a body at the cap must still be appended:/n%.200s", got)
	}
	if strings.Contains(got, "too large to re-append") {
		t.Fatalf("cap must be inclusive, not exclusive:/n%.200s", got)
	}
}

func TestDelegatedPromptDropsOversizeContract(t *testing.T) {
	t.Setenv(DelegationSpecEnv, "on")
	contract := "## Enforced workspace change contract\n\n" + strings.Repeat("z", DelegationContractMaxBytes+1)
	h := newSpecHarness("Body.\n\n---\n\n" + contract)

	got := h.delegatedPromptWithSpec("summary")

	if strings.Contains(got, "### Verbatim — workspace change contract") {
		t.Fatalf("an oversize contract section must be dropped:/n%.200s", got)
	}
	if !strings.Contains(got, "### Verbatim — task specification") {
		t.Fatalf("dropping the contract must not drop the body:/n%.200s", got)
	}
}
