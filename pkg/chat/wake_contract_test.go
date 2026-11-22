package chat

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/daybeam/vortex/core"
)

// The wake turn's contract, pinned at the layer that decides what it costs.
//
// The four consumers are tested one by one on purpose. A single
// "observation now says 1 instead of na" test would pass on the broken version:
// filling in declaredArtifacts also arms contractArmed(), which appends a steer
// to the last tool result, which is a treatment. §6.3d's falsification line is
// exactly that confusion, so each consumer gets its own assertion and the
// observe mode has to keep three of the four silent.

// wakeTrigger is the real one: followUpTriggerFmt rendered, i.e. a sentence with
// no "[COMPLETION CONTRACT]" section. Copied rather than imported because the
// format lives in pkg/api, and the point of this file is that a sentence like
// this parses to nothing.
const wakeTrigger = "[System notice] Delegated task task_abc has finished; its output is in the workspace. " +
	"Read the artifacts it produced and answer the user's original request now. " +
	"Do not re-run the task from scratch, and do not rewrite the artifacts it produced — " +
	"they are the deliverable. Only create a file if the task produced none."

// seededHarness mirrors what Run does: parse the trigger, then publish the
// result on the harness the way chat_harness.go:251-252 does.
func seededHarness(root string, declared []string, mode WakeContractMode) *ChatHarness {
	h := &ChatHarness{SessionRoot: root}
	h.SeedWakeContract(declared, mode)
	h.declaredArtifacts = h.initContract(wakeTrigger)
	h.declaredFields = core.ParseDeclaredArtifactFields(wakeTrigger)
	return h
}

// Off is the only mode that must leave no trace at all — not "behaves the same",
// but "cannot have been given a declaration". A field left behind on an off run
// is a number the next reader will mistake for a measurement.
func TestWakeContract_OffStoresNothing(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "capacity_plan.json")
	h := seededHarness(root, []string{"capacity_plan.json"}, WakeContractOff)

	if h.contract.wakeDeclared != nil || h.contract.wakeMode != WakeContractOff {
		t.Fatalf("off must not store the seed, got declared=%v mode=%v",
			h.contract.wakeDeclared, h.contract.wakeMode)
	}
	if h.contract.wakeSeeded {
		t.Fatal("off must not mark the turn as seeded")
	}
	if len(h.declaredArtifacts) != 0 {
		t.Fatalf("a wake trigger declares nothing, got %v", h.declaredArtifacts)
	}
	want := "contract_present=na contract_missing=na contract_defects=na checks_armed=na"
	if got := h.contractObservation(); got != want {
		t.Fatalf("off changed the summary line:\n got %q\nwant %q", got, want)
	}
	if h.contractArmed() {
		t.Fatal("off must leave the completion block inert")
	}
}

// The whole point of the observe mode: the summary line becomes a number and
// nothing else moves.
func TestWakeContract_ObserveFillsObservationOnly(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "capacity_plan.json")

	h := seededHarness(root, []string{"capacity_plan.json"}, WakeContractObserve)
	if !h.contract.wakeSeeded {
		t.Fatal("observe must mark the turn as seeded — that is what holds the other three consumers back")
	}
	if len(h.declaredArtifacts) != 1 {
		t.Fatalf("the declaration must reach the run, got %v", h.declaredArtifacts)
	}

	got := h.contractObservation()
	if want := "contract_present=1 contract_missing=0"; !strings.HasPrefix(got, want) {
		t.Fatalf("observation = %q, want it to open with %q", got, want)
	}

	// Consumer 2: the one-shot done steer and the bounded defect steer.
	if h.contractArmed() {
		t.Fatal("observe armed the completion block; the summary line is the only channel this mode may use")
	}
	// Consumer 3: delegate_to_orchestrator's skip.
	if h.completionEnforced() {
		t.Fatal("observe must not let presence skip a delegation")
	}
	// Consumer 4: the status answer's terminal action.
	if h.declaredForStatus() != nil {
		t.Fatalf("observe must not quote counts in a status answer, got %v", h.declaredForStatus())
	}
}

// A missing artifact is the case that would produce a defect steer if observe
// ever leaked into the armed path: it is truthful on the summary line and it is
// an accusation in a tool result.
func TestWakeContract_ObserveReportsMissingWithoutAccusing(t *testing.T) {
	root := t.TempDir() // nothing planted
	h := seededHarness(root, []string{"capacity_plan.json"}, WakeContractObserve)

	got := h.contractObservation()
	if want := "contract_present=0 contract_missing=1"; !strings.HasPrefix(got, want) {
		t.Fatalf("observation = %q, want it to open with %q", got, want)
	}
	if h.contractArmed() {
		t.Fatal("a missing artifact in observe mode must not arm the defect steer")
	}
}

func TestWakeContract_FullArmsTheBlock(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "capacity_plan.json")
	h := seededHarness(root, []string{"capacity_plan.json"}, WakeContractFull)

	if !h.contractArmed() {
		t.Fatal("full must arm the completion block")
	}
	if !h.completionEnforced() {
		t.Fatal("full must let presence act")
	}
	if d := h.declaredForStatus(); len(d) != 1 {
		t.Fatalf("full must quote the declaration in a status answer, got %v", d)
	}
}

// Full mode can tell the wake turn "everything is present". It can never tell it
// "present and wrong", and that is not a policy choice — the shape half of the
// contract is parsed out of a "Create `path` containing:" block that a wake
// trigger does not carry, so declaredFields is empty and there is nothing to
// judge against.
//
// Pinned because §6.3d lists contract_defect_steer as the signal that the arm
// turned into treatment. On a wake turn it cannot fire, so a reader who does not
// know that reads defect_steer=0 as "the artifacts were clean".
func TestWakeContract_FullCanOnlySayDone(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "capacity_plan.json")
	h := seededHarness(root, []string{"capacity_plan.json"}, WakeContractFull)

	if len(h.declaredFields) != 0 {
		t.Fatalf("a wake trigger declares no fields, got %v", h.declaredFields)
	}
	defects := h.artifactDefects(context.Background(), "task_wc", func(ChatEvent) error { return nil })
	if len(defects) != 0 {
		t.Fatalf("a wake turn produced defects, so the defect steer could fire: %v", defects)
	}
}

// The seed is a fallback, not an override. A prompt that states its own contract
// keeps it — otherwise a turn that inherited a task's declaration would silently
// replace what the user asked for, and the 274 front turns would change too.
func TestWakeContract_PromptContractWins(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "capacity_plan.json")

	h := &ChatHarness{SessionRoot: root}
	h.SeedWakeContract([]string{"capacity_plan.json"}, WakeContractObserve)
	h.declaredArtifacts = h.initContract(createContract)

	if h.contract.wakeSeeded {
		t.Fatal("a prompt with its own contract must not be marked as seeded")
	}
	if got, want := h.declaredArtifacts, []string{"report.json"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("the prompt's contract was replaced: got %v, want %v", got, want)
	}
	// An ordinary turn is never held back, whatever mode the process carries.
	if !h.contractArmed() {
		t.Fatal("an ordinary turn with a declaration must keep arming the block")
	}
}

// Consumer 3, exercised rather than inferred: the same harness that would answer
// "skip" in full mode must actually delegate in observe mode.
func TestWakeContract_ObserveStillDelegates(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "capacity_plan.json")

	calls := 0
	delegate := func(prompt string) (string, error) { calls++; return "task_gate0", nil }

	for _, tc := range []struct {
		mode WakeContractMode
		skip bool
		why  string
	}{
		{WakeContractObserve, false, "observe may not answer with the skip marker: that is a treatment, not a measurement"},
		{WakeContractFull, true, "full is the arm where presence is allowed to stop a redundant hand-off"},
	} {
		calls = 0
		h := seededHarness(root, []string{"capacity_plan.json"}, tc.mode)
		h.Delegate = delegate

		out, err := h.execTool(context.Background(), "delegate_to_orchestrator",
			map[string]any{"prompt": "reconcile the invoices"}, "chat_wakec")
		if err != nil {
			t.Fatalf("%v: execTool returned an error: %v", tc.mode, err)
		}
		got := strings.HasPrefix(out, core.MarkerDelegationSkipped)
		if got != tc.skip {
			t.Fatalf("mode=%v skip=%v, want %v (%s), got:\n%s", tc.mode, got, tc.skip, tc.why, out)
		}
		if tc.skip && calls != 0 {
			t.Fatalf("skip marker returned but the task was submitted anyway (%d calls)", calls)
		}
		if !tc.skip && calls != 1 {
			t.Fatalf("mode=%v must submit exactly once, got %d", tc.mode, calls)
		}
	}
}

// Consumer 4, exercised: the status answer a wake turn gets must read exactly as
// it did before when the mode is observe.
func TestWakeContract_ObserveKeepsStatusAnswer(t *testing.T) {
	root := t.TempDir()
	plant(t, root, "capacity_plan.json")
	status := map[string]any{"status": "completed", "workspace_root": root}

	observe := seededHarness(root, []string{"capacity_plan.json"}, WakeContractObserve)
	old := formatTaskStatus("task_abc", status, nil)
	got := formatTaskStatus("task_abc", status, observe.declaredForStatus())
	if got != old {
		t.Fatalf("observe changed the status answer:\n got:\n%s\nwant:\n%s", got, old)
	}
	if strings.Contains(got, "declared 1 artifact") {
		t.Fatalf("observe leaked the artifact count into the status answer:\n%s", got)
	}

	// The same answer in full mode does carry the counts, which is what makes the
	// line above a real assertion rather than a string that never appears.
	full := seededHarness(root, []string{"capacity_plan.json"}, WakeContractFull)
	if !strings.Contains(formatTaskStatus("task_abc", status, full.declaredForStatus()), "declared 1 artifact") {
		t.Fatal("full mode must quote the counts; otherwise the observe assertion proves nothing")
	}
}

// Seeding with nothing to declare is not an error and must not create a
// contract: presence computed against an empty list is 0/0, which reads as
// "everything delivered".
func TestWakeContract_EmptyDeclarationStoresNothing(t *testing.T) {
	h := &ChatHarness{SessionRoot: t.TempDir()}
	h.SeedWakeContract(nil, WakeContractFull)
	if h.contract.wakeMode != WakeContractOff {
		t.Fatalf("an empty declaration must leave the mode off, got %v", h.contract.wakeMode)
	}
	h.declaredArtifacts = h.initContract(wakeTrigger)
	if got := h.contractObservation(); !strings.Contains(got, "contract_present=na") {
		t.Fatalf("observation = %q, want na", got)
	}
}

// The declared slice is copied, because the caller's slice comes from a task
// outcome that is still owned by the notification path.
func TestWakeContract_CopiesTheDeclaration(t *testing.T) {
	src := []string{"a.json"}
	h := &ChatHarness{}
	h.SeedWakeContract(src, WakeContractObserve)
	src[0] = "mutated"
	if h.contract.wakeDeclared[0] != "a.json" {
		t.Fatalf("the seed aliased the caller's slice: %v", h.contract.wakeDeclared)
	}
	_ = fmt.Sprintf("%v", h.contract.wakeDeclared)
}
