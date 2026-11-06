package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// Q2-4 (2026-11-06): SOP exit_criteria prose is forbidden by the authoring
// contract, but files that already carry it must keep loading; the loader warns
// instead of rejecting. These tests pin both halves: the warning fires for
// prose and stays quiet for the three declarative forms.

func writeSOPFixture(t *testing.T, dir, id, criteria string) {
	t.Helper()
	sopsDir := filepath.Join(dir, "workspace", "sops")
	if err := os.MkdirAll(sopsDir, 0o755); err != nil {
		t.Fatalf("mkdir sops: %v", err)
	}
	sop := map[string]any{
		"id":       id,
		"version":  "1.0",
		"author":   "human_expert",
		"triggers": []any{},
		"steps": map[string]any{
			"only": map[string]any{
				"id":            "only",
				"role":          "worker",
				"task":          "do the thing",
				"exit_criteria": criteria,
			},
		},
	}
	data, err := json.MarshalIndent(sop, "", "  ")
	if err != nil {
		t.Fatalf("marshal sop: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sopsDir, id+".json"), data, 0o644); err != nil {
		t.Fatalf("write sop: %v", err)
	}
}

func writeSplitConfigFixture(t *testing.T, dir string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "config_test.json")
	cfg := map[string]any{
		"_split_layout": map[string]any{"version": 2, "sops": "workspace/sops/"},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

func TestSOPLoadWarnsOnProseExitCriteria(t *testing.T) {
	dir := t.TempDir()
	writeSOPFixture(t, dir, "legacy_sop", "SOP saved successfully and validateSOPDAG passes")
	cfgPath := writeSplitConfigFixture(t, dir)

	buf, restore := captureLog(t)
	r, err := NewRegistry(cfgPath)
	restore()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// The warning must not turn into a rejection: the SOP still loads.
	if _, ok := r.SOPs["legacy_sop"]; !ok {
		t.Fatal("legacy SOP with prose exit_criteria did not load; the warning must not reject")
	}

	out := buf.String()
	for _, want := range []string{
		`SOP "legacy_sop"`,
		`only="SOP saved successfully and validateSOPDAG passes"`,
		"Q2-4",
	} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("warning missing %q; got:\n%s", want, out)
		}
	}
}

func TestSOPLoadStaysQuietForDeclarativeCriteria(t *testing.T) {
	for name, criteria := range map[string]string{
		"empty": "",
		"json":  "json",
		"regex": `regex:https?://.*\.png`,
		"file":  "file:workspace/tmp/report.md",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeSOPFixture(t, dir, "clean_sop", criteria)
			cfgPath := writeSplitConfigFixture(t, dir)

			buf, restore := captureLog(t)
			r, err := NewRegistry(cfgPath)
			restore()
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			if _, ok := r.SOPs["clean_sop"]; !ok {
				t.Fatal("SOP did not load")
			}
			if bytes.Contains(buf.Bytes(), []byte("non-declarative exit_criteria")) {
				t.Errorf("declarative criteria %q triggered the prose warning:\n%s", criteria, buf.String())
			}
		})
	}
}

// TestBuiltinSOPExitCriteriaAreDeclarative keeps the seeded copy of
// sop_auto_generate in sync with the workspace file: both must carry the same
// file: gate on the save step, or a fresh install reseeds the prose criterion
// that Q2-4 removed.
func TestBuiltinSOPExitCriteriaAreDeclarative(t *testing.T) {
	var found *schemas.SOP
	for i := range builtinSOPs {
		if builtinSOPs[i].ID == "sop_auto_generate" {
			found = &builtinSOPs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("builtin sop_auto_generate not found")
	}
	save, ok := found.Steps["save"]
	if !ok {
		t.Fatal("builtin sop_auto_generate has no save step")
	}
	if !schemas.ExitCriteriaIsDeclarative(save.ExitCriteria) {
		t.Fatalf("builtin save exit_criteria %q is not declarative; Q2-4 removed the prose form", save.ExitCriteria)
	}
	if want := "file:workspace/tmp/sop_auto_generate.saved.json"; save.ExitCriteria != want {
		t.Fatalf("builtin save exit_criteria = %q, want %q", save.ExitCriteria, want)
	}
}
