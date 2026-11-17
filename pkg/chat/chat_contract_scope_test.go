package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/core"
)

const (
	modifyOnlyContract = "## Enforced workspace change contract\n" +
		"- Create: none\n- Modify: `submission/service.py`\n- Delete: none\n"
	createContract = "## Enforced workspace change contract\n" +
		"- Create: `report.json`\n- Modify: none\n- Delete: none\n"
	neitherContract = "## Enforced workspace change contract\n" +
		"- Create: none\n- Modify: none\n- Delete: none\n"
)

// seedModifyOnlyWorkspace writes 263's seed shape: the deliverable already
// exists, which is the whole reason this check exists (→221).
func seedModifyOnlyWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "submission")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "service.py"), []byte("raise NotImplementedError\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// 263's measured failure, reproduced at the unit level: a working deliverable
// plus a runtime database left in the workspace. The scorer's workspace_scope
// compares the directory to the seed, so `state.db` alone turns a correct
// service into 0.0.
func TestContractScope_ModifyOnlyReportsLeftovers(t *testing.T) {
	root := seedModifyOnlyWorkspace(t)
	h := &ChatHarness{SessionRoot: root}

	if got := h.initContract(modifyOnlyContract); len(got) != 0 {
		t.Fatalf("modify-only must declare no artifacts, got %v", got)
	}
	h.declaredArtifacts = nil // Run does this on the next line

	if !h.extrasArmed() {
		t.Fatal("modify-only contract must arm the extras check")
	}
	if !h.contractArmed() {
		t.Fatal("a captured baseline must arm the completion block")
	}
	if d := h.newPathDefects(); len(d) != 0 {
		t.Fatalf("the untouched seed reported extras: %v", d)
	}
	if h.contract.lastExtra != nil {
		t.Fatalf("lastExtra must stay nil on a clean workspace, got %v", h.contract.lastExtra)
	}

	// The model puts its state database next to the deliverable.
	if err := os.WriteFile(filepath.Join(root, "state.db"), []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := h.newPathDefects()
	if len(d) != 1 {
		t.Fatalf("want one defect line for the leftover, got %v", d)
	}
	if want := []string{"state.db"}; len(h.contract.lastExtra) != 1 || h.contract.lastExtra[0] != want[0] {
		t.Fatalf("lastExtra = %v, want %v", h.contract.lastExtra, want)
	}
	marker := core.ContractDefectsMarker(d, h.contract.lastExtra)
	if marker == "" {
		t.Fatal("the defect must render a steer")
	}
	for _, want := range []string{"allows no created paths", "state.db"} {
		if !strings.Contains(marker, want) {
			t.Errorf("steer %q does not mention %q", marker, want)
		}
	}
}

// 166 + 102 tasks create something, so their completion signal is the declared
// artifact one and must not change. A file appearing there is not a defect this
// check may report — 268 tasks have to behave exactly as before.
func TestContractScope_CreateShapedTasksAreUntouched(t *testing.T) {
	root := t.TempDir()
	h := &ChatHarness{SessionRoot: root}
	if got := h.initContract(createContract); len(got) != 1 {
		t.Fatalf("want the declared artifact, got %v", got)
	}
	h.declaredArtifacts = []string{"report.json"}

	if h.extrasArmed() {
		t.Fatal("a contract that creates something must not arm the extras check")
	}
	if !h.contractArmed() {
		t.Fatal("the declared-artifact path must still arm the completion block")
	}
	if err := os.WriteFile(filepath.Join(root, "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := h.newPathDefects(); len(d) != 0 {
		t.Fatalf("create-shaped task reported extras: %v", d)
	}
	// No snapshot was taken, which is the cheapest proof that this path costs
	// the other 268 tasks nothing.
	if h.contract.baseline != nil {
		t.Fatal("create-shaped tasks must not pay for a workspace walk")
	}
}

// Three tasks have neither column. They get no extras check either: there is no
// allowed set to compare against, and inventing one would be a gate nobody
// asked for.
func TestContractScope_NeitherShapeIsInert(t *testing.T) {
	h := &ChatHarness{SessionRoot: t.TempDir()}
	h.initContract(neitherContract)
	if h.extrasArmed() || h.contractArmed() {
		t.Fatal("Create:none / Modify:none must leave both gates closed")
	}
}

// Outside the benchmark there is no contract section at all, and the harness is
// also used with SessionRoot == "" (the legacy outputs/<taskID> layout). Both
// must leave every gate closed rather than resolve names against the engine's
// own working directory.
func TestContractScope_NoContractOrNoRootIsInert(t *testing.T) {
	h := &ChatHarness{SessionRoot: t.TempDir()}
	h.initContract("just an ordinary prompt")
	if h.extrasArmed() || h.contractArmed() || h.newPathDefects() != nil {
		t.Fatal("no contract section must leave the extras check inert")
	}

	legacy := &ChatHarness{}
	legacy.initContract(modifyOnlyContract)
	if legacy.contractArmed() {
		t.Fatal("SessionRoot == \"\" must not arm the check: there is no tree to diff")
	}
	if d := legacy.newPathDefects(); d != nil {
		t.Fatalf("no root must report nothing, got %v", d)
	}
}
