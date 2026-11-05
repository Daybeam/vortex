package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCompletionChecks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("report.sql.check.py", "import sys\nsys.exit(0)\n")
	write("empty.md.check.py", "  \n")
	write("notes.md", "not a check")

	cases := []struct {
		name      string
		dir       string
		artifacts []string
		want      int
	}{
		{"matching artifact", dir, []string{"report.sql"}, 1},
		{"no declaration", dir, []string{"notes.md"}, 0}, // file exists but has no .check.py
		{"empty check is not a check", dir, []string{"empty.md"}, 0},
		{"unconfigured dir", "", []string{"report.sql"}, 0}, // inert by default
		{"duplicate artifacts collapse", dir, []string{"report.sql", "report.sql"}, 1},
		{"path is flattened to a base name", dir, []string{"./sub/report.sql"}, 1},
		{"missing check", dir, []string{"absent.sql"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LoadCompletionChecks(tc.dir, tc.artifacts)
			if len(got) != tc.want {
				t.Fatalf("got %d checks, want %d (%v)", len(got), tc.want, got)
			}
			if tc.want == 1 && got[0].Artifact != "report.sql" {
				t.Fatalf("artifact = %q, want report.sql", got[0].Artifact)
			}
		})
	}
}

// The prefix is the only channel for the artifact name: execute_code runs a
// program with no argv and no environment of its own.
func TestCompletionCheckProgram(t *testing.T) {
	got := CompletionCheckProgram(CompletionCheck{Artifact: "report.sql", Source: "raise SystemExit(0)\n"})
	if !strings.HasPrefix(got, `ARTIFACT = "report.sql"`+"\n") {
		t.Fatalf("missing artifact prefix: %q", got)
	}
	if !strings.HasSuffix(got, "raise SystemExit(0)\n") {
		t.Fatalf("body was altered: %q", got)
	}
}
