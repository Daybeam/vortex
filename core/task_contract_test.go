package core

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// contractSample reproduces what tools/run_benchmark.py:942-956 actually emits.
//
// The contract section is byte-faithful: that renderer appends exactly this
// heading, this two-sentence preamble, one "- Create/Modify/Delete:" line per
// field (backticked paths joined by ", ", or the literal "none"), and the
// closing paragraph -- see task 029's mutation_contract, whose allowed_created
// is these four names in this order.
//
// The prose before it is a condensed stand-in for 029's instruction.md, not a
// transcription. What it preserves is the property under test: the real
// instruction is dense with backticked identifiers (`order_count`, `order_id`,
// `order_status`) and names customer_metrics.csv in plain prose at lines 12, 17
// and 54 -- so "every backticked token in the text" and "every filename-looking
// token in the text" are both wrong answers, and the only correct one is
// "the - Create: line inside the contract section".
const contractSample = "## Your task\n\n" +
	"Build customer-level revenue metrics and a reconciliation note from local tables.\n\n" +
	"Inputs copied to the workspace root:\n" +
	"- $WORKSPACE/customers.csv\n" +
	"- $WORKSPACE/orders.csv\n\n" +
	"Rules: `order_count` counts distinct `order_id` values whose `order_status`\n\n" +
	"## Enforced workspace change contract\n\n" +
	"The controller compares the final state of `/workspace` after every round " +
	"with that round's starting state. At the end of the round, only these " +
	"final-state differences may remain:\n\n" +
	"- Create: `customer_metrics.csv`, `reconciliation_audit.json`, `reconciliation_notes.md`, `region_summary.json`\n" +
	"- Modify: none\n" +
	"- Delete: none\n\n" +
	"Do not leave any other created, modified, or deleted path in `/workspace`.\n"

func TestParseDeclaredArtifacts_RealSample(t *testing.T) {
	got := ParseDeclaredArtifacts(contractSample)
	want := []string{
		"customer_metrics.csv",
		"reconciliation_audit.json",
		"reconciliation_notes.md",
		"region_summary.json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Backticked paths outside the section must not be picked up. The guard that
// does the work here is the "- Create:" prefix, not section scoping — prose
// lines do not start with it. Mutation-checked: removing the prefix test makes
// this one and RealSample fail; removing section scoping only fails
// StopsAtNextHeading, which is the case that isolates scoping.
func TestParseDeclaredArtifacts_IgnoresBackticksOutsideSection(t *testing.T) {
	text := "Read from `/workspace/ledger.csv` and `/workspace/vendor_map.csv`.\n\n" +
		"## Enforced workspace change contract\n\n" +
		"- Create: `result.json`\n" +
		"- Modify: none\n" +
		"- Delete: none\n"
	got := ParseDeclaredArtifacts(text)
	if !reflect.DeepEqual(got, []string{"result.json"}) {
		t.Fatalf("got %v, want [result.json]", got)
	}
}

func TestParseDeclaredArtifacts_NoSectionIsNil(t *testing.T) {
	if got := ParseDeclaredArtifacts("Just a normal task with `code` in it."); got != nil {
		t.Fatalf("no contract section must yield nil, got %v", got)
	}
}

// "- Modify: none" and "- Delete: none" are lines inside the section. Parsing
// every line that mentions a backtick, or every line starting with "- ",
// would produce the artifact "none" here.
func TestParseDeclaredArtifacts_NoneIsNotAnArtifact(t *testing.T) {
	text := "## Enforced workspace change contract\n" +
		"- Create: none\n" +
		"- Modify: `existing.csv`\n" +
		"- Delete: none\n"
	if got := ParseDeclaredArtifacts(text); len(got) != 0 {
		t.Fatalf("want no artifacts, got %v", got)
	}
}

func TestParseDeclaredArtifacts_MergesCreateLinesAndDedupes(t *testing.T) {
	text := "## Enforced workspace change contract\n" +
		"- Create: `a.csv`, `b.json`\n" +
		"- Create: `b.json`, `c.md`\n"
	got := ParseDeclaredArtifacts(text)
	want := []string{"a.csv", "b.json", "c.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (dedup, first-seen order)", got, want)
	}
}

// The section ends at the next "## " heading. A later section that also has a
// "- Create:" line is a different contract and must not leak in.
func TestParseDeclaredArtifacts_StopsAtNextHeading(t *testing.T) {
	text := "## Enforced workspace change contract\n" +
		"- Create: `real.csv`\n\n" +
		"## Appendix\n" +
		"- Create: `example_only.csv`\n"
	got := ParseDeclaredArtifacts(text)
	if !reflect.DeepEqual(got, []string{"real.csv"}) {
		t.Fatalf("got %v, want [real.csv]", got)
	}
}

func TestWorkspaceArtifactsComplete_Counts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "present.csv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A zero-byte file is still a file: presence is the question here, not
	// content quality.
	if err := os.WriteFile(filepath.Join(root, "empty.json"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory named like an artifact must not count as one.
	if err := os.Mkdir(filepath.Join(root, "adir.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	present, missing := WorkspaceArtifactsComplete(root, []string{
		"present.csv", "empty.json", "absent.csv", "adir.md",
	})
	if present != 2 || missing != 2 {
		t.Fatalf("present=%d missing=%d, want 2/2", present, missing)
	}
}

// The contract text comes from the task prompt. A path that escapes the
// workspace must be counted missing, never followed.
func TestWorkspaceArtifactsComplete_RefusesEscapingPaths(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside.csv")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	present, missing := WorkspaceArtifactsComplete(root, []string{
		"../outside.csv", "/etc/passwd", "",
	})
	if present != 0 || missing != 3 {
		t.Fatalf("present=%d missing=%d, want 0/3", present, missing)
	}
}

// "/workspace/x.csv" appears in prompt prose; treating it as a relative name
// keeps the parser usable on either form.
func TestWorkspaceArtifactsComplete_AcceptsWorkspacePrefix(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.csv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	present, missing := WorkspaceArtifactsComplete(root, []string{"/workspace/x.csv"})
	if present != 1 || missing != 0 {
		t.Fatalf("present=%d missing=%d, want 1/0", present, missing)
	}
}

// fieldsSample is 011's real prompt block, verbatim except for the surrounding
// prose. It is the fixture that proves the scoped-bullet rule: `start`, `end`
// and `selection_reason` live inside "for each candidate:", so a parser that
// collected every backticked token would demand them at the top level and every
// compliant plan would be reported as broken.
const fieldsSample = "Apply every constraint in the request.\n\n" +
	"Create `$WORKSPACE/scheduling_plan.json` containing:\n\n" +
	"- `request_id`, `status`, and `timezone`;\n" +
	"- one `recommended` candidate and two ordered `fallbacks`;\n" +
	"- for each candidate: `start`, `end`, and a concise `selection_reason`;\n" +
	"- a `participant_local_times` object for the recommended candidate, keyed by\n" +
	"  participant email, with a human-readable local date and start/end time;\n" +
	"- `evidence`, listing the workspace-relative request/calendar files used.\n\n" +
	"Draft `$WORKSPACE/customer_reply.md` to Dana.\n"

func TestParseDeclaredArtifactFields_RealSample(t *testing.T) {
	got := ParseDeclaredArtifactFields(fieldsSample)
	want := []string{
		"request_id", "status", "timezone",
		"recommended", "fallbacks",
		"participant_local_times", "evidence",
	}
	if !reflect.DeepEqual(got["scheduling_plan.json"], want) {
		t.Fatalf("fields = %#v, want %#v", got["scheduling_plan.json"], want)
	}
	for _, scoped := range []string{"start", "end", "selection_reason"} {
		for _, field := range got["scheduling_plan.json"] {
			if field == scoped {
				t.Fatalf("%q is a per-candidate field and must not become a top-level requirement", scoped)
			}
		}
	}
	if _, ok := got["customer_reply.md"]; ok {
		t.Error("only a `Create ... containing:` block declares fields; a prose draft instruction must not")
	}
}

func TestParseDeclaredArtifactFields_NoBlockIsNil(t *testing.T) {
	// nil, not an empty map: callers use nil to mean "behave exactly as before".
	if got := ParseDeclaredArtifactFields(contractSample); got != nil {
		t.Fatalf("fields = %#v, want nil for a prompt that declares no field block", got)
	}
}

// planFixture writes body to plan.json under a fresh root and returns the root.
func planFixture(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "plan.json"), []byte(body), 0644); err != nil {
		t.Fatalf("write plan.json: %v", err)
	}
	return root
}

func TestWorkspaceArtifactDefects(t *testing.T) {
	declared := map[string][]string{"plan.json": {"recommended", "fallbacks", "evidence"}}

	tests := []struct {
		name       string
		root       func(t *testing.T) string
		fields     map[string][]string
		wantSubstr []string
		wantNone   bool
	}{
		{
			name: "correct shape",
			root: func(t *testing.T) string {
				return planFixture(t, `{"recommended":{},"fallbacks":[],"evidence":[]}`)
			},
			fields:   declared,
			wantNone: true,
		},
		{
			// The v71 failure, verbatim: every window correct, parked under a
			// key the task never asked for. Existence called this done.
			name: "candidates array instead of recommended",
			root: func(t *testing.T) string {
				return planFixture(t, `{"candidates":[{"type":"recommended","start":"2026-05-12T11:30:00-04:00"}]}`)
			},
			fields:     declared,
			wantSubstr: []string{`plan.json is missing "recommended"`, `plan.json is missing "fallbacks"`, `plan.json is missing "evidence"`},
		},
		{
			name: "not json",
			root: func(t *testing.T) string {
				return planFixture(t, "recommended: 11:30 - 12:15\n")
			},
			fields:     declared,
			wantSubstr: []string{"plan.json is not a JSON object"},
		},
		{
			// Absence has its own message (WorkspaceArtifactsComplete). Reporting
			// it twice would make one missing file look like four problems.
			name:     "missing file is not a defect",
			root:     func(t *testing.T) string { return t.TempDir() },
			fields:   declared,
			wantNone: true,
		},
		{
			// Reverse case (→122): the same wrong file with no declared fields
			// must yield nothing, otherwise the test above would pass even if the
			// check never consulted the declaration at all.
			name: "wrong file without a declaration",
			root: func(t *testing.T) string {
				return planFixture(t, `{"candidates":[]}`)
			},
			fields:   nil,
			wantNone: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WorkspaceArtifactDefects(tc.root(t), tc.fields)
			if tc.wantNone {
				if len(got) != 0 {
					t.Fatalf("defects = %#v, want none", got)
				}
				return
			}
			joined := strings.Join(got, " | ")
			for _, want := range tc.wantSubstr {
				if !strings.Contains(joined, want) {
					t.Errorf("defects = %q, want it to contain %q", joined, want)
				}
			}
		})
	}
}
