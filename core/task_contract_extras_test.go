package core

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ParseDeclaredContract is the column-split version of the section parser. The
// TestParseDeclaredArtifacts_* table (via declaredCreates) already pins what
// Create yields; this pins
// the other two columns, because they are what the extras gate reads and a
// mis-split there would silently widen or narrow the allowed set.
func TestParseDeclaredContract_Columns(t *testing.T) {
	tests := []struct {
		name string
		body string
		want DeclaredContract
	}{
		{
			name: "create and modify",
			body: "- Create: `a.csv`\n- Modify: `b.csv`\n- Delete: `c.csv`\n",
			want: DeclaredContract{Present: true,
				Create: []string{"a.csv"}, Modify: []string{"b.csv"}, Delete: []string{"c.csv"}},
		},
		{
			name: "create only",
			body: "- Create: `a.csv`\n- Modify: none\n- Delete: none\n",
			want: DeclaredContract{Present: true, Create: []string{"a.csv"}},
		},
		{
			// 262/263/264: the only shape where the deliverable is named on the
			// Modify line and nowhere else.
			name: "modify only",
			body: "- Create: none\n- Modify: `submission/service.py`\n- Delete: none\n",
			want: DeclaredContract{Present: true, Modify: []string{"submission/service.py"}},
		},
		{
			name: "neither",
			body: "- Create: none\n- Modify: none\n- Delete: none\n",
			want: DeclaredContract{Present: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseDeclaredContract("## Enforced workspace change contract\n" + tc.body)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}

	// Present is the bit that separates "no section" from "section names
	// nothing to create". Without it the caller cannot tell the two apart, which
	// is what →219 recorded and what the extras gate needs (→208: a parsed bit
	// with no consumer is dead weight, so this one has a test naming its use).
	if got := ParseDeclaredContract("just prose"); got.Present {
		t.Fatalf("no section must not report Present, got %#v", got)
	}
	if got := ParseDeclaredContract("## Enforced workspace change contract\n- Create: none\n"); !got.Present {
		t.Fatal("a section that names no creates must still report Present")
	}
}

// order-independent construction of a workspace, shared by the two tests below.
func writeWorkspace(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// The extras check answers "what did this run add". The three properties that
// matter are: a path that was there before is never reported, a path that
// appeared is reported whether it is a file or a directory, and the ignored
// housekeeping directories never enter the comparison.
func TestWorkspaceNewPaths(t *testing.T) {
	root := t.TempDir()
	// 263's seed shape, abbreviated: the deliverable and its directory already
	// exist, so neither may ever show up as "new".
	writeWorkspace(t, root, "task.md", "submission/service.py", "seed.db")

	baseline, err := SnapshotWorkspace(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(baseline) != 4 { // task.md, submission, submission/service.py, seed.db
		t.Fatalf("baseline = %v, want the 4 seeded entries", baseline)
	}

	if got, err := WorkspaceNewPaths(root, baseline); err != nil || len(got) != 0 {
		t.Fatalf("unchanged workspace: got %v err %v, want none", got, err)
	}

	writeWorkspace(t, root, "state.db", "logs/run.log", ".git/config", "node_modules/x/index.js")
	got, err := WorkspaceNewPaths(root, baseline)
	if err != nil {
		t.Fatalf("new paths: %v", err)
	}
	want := []string{"logs", "logs/run.log", "state.db"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (sorted; .git and node_modules skipped)", got, want)
	}

	// Deleting a seeded file is a different violation with a different message
	// (MissingArtifacts). It must not surface here as "new".
	if err := os.Remove(filepath.Join(root, "task.md")); err != nil {
		t.Fatal(err)
	}
	got, err = WorkspaceNewPaths(root, baseline)
	if err != nil {
		t.Fatalf("after delete: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a removed seed file changed the extras list: %v", got)
	}
}

func TestSnapshotWorkspace_MissingRootIsAnError(t *testing.T) {
	if _, err := SnapshotWorkspace(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing root must be an error: a caller that treats it as an empty " +
			"baseline would report every seed file as created")
	}
}

// The two defect kinds need different repairs, so they get different sentences.
// "Fix the artifacts" is wrong when nothing is wrong with the artifact and the
// leftovers are the problem (→185: a gate may only enforce what the task says,
// and this one quotes the task's own /runtime // /tmp instruction).
func TestContractDefectsMarker(t *testing.T) {
	fieldDefect := []string{`plan.json is missing "recommended"`}
	if got := ContractDefectsMarker(fieldDefect, nil); !strings.Contains(got, "do not satisfy the contract") {
		t.Fatalf("no extras must fall through to the field sentence, got %q", got)
	}
	if got := ContractDefectsMarker(nil, nil); got != "" {
		t.Fatalf("no defects at all must render nothing, got %q", got)
	}

	got := ContractDefectsMarker(nil, []string{"state.db", "logs"})
	for _, want := range []string{"allows no created paths", "state.db, logs", "/runtime", "/tmp"} {
		if !strings.Contains(got, want) {
			t.Errorf("marker %q does not mention %q", got, want)
		}
	}

	// Bounded like the field marker: seven extras must not become seven entries.
	long := []string{"a", "b", "c", "d", "e", "f", "g"}
	if got := ContractDefectsMarker(nil, long); !strings.Contains(got, "and 1 more") {
		t.Errorf("marker must cap the list, got %q", got)
	}
}
