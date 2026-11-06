package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The delegation receipt used to say nothing about the files a task declared:
// `names` comes from the subagent's self-reported output_files, and the delegate
// contract has no field for those, so every delegated task took the "declared no
// output files, go ls" branch. harness-bench 011 b3d-v71-r5 then spent 32
// execute_code calls looking for a deliverable that had landed one directory too
// deep. These tests pin the counts, the names, and the strict landing rule.

func writeDeclaredArtifact(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func TestFormatTaskStatus_ReportsDeclaredPresentAndMissing(t *testing.T) {
	root := t.TempDir()
	writeDeclaredArtifact(t, root, "scheduling_plan.json", "{}")
	declared := []string{"scheduling_plan.json", "customer_reply.md"}

	out := formatTaskStatus("task_1", map[string]any{
		"status":         "completed",
		"workspace_root": root,
	}, declared)

	if !strings.Contains(out, "declared 2 artifact(s); present 1") {
		t.Fatalf("the receipt must count what exists, got:\n%s", out)
	}
	if !strings.Contains(out, "missing: customer_reply.md") {
		t.Errorf("the receipt must name the missing artifact, got:\n%s", out)
	}
	if strings.Contains(out, "scheduling_plan.json,") || strings.Contains(out, "missing: scheduling_plan.json") {
		t.Errorf("a present artifact must not be listed as missing, got:\n%s", out)
	}
	if strings.Contains(out, "read them and answer") {
		t.Errorf("the answer instruction belongs to the all-present branch only, got:\n%s", out)
	}
}

func TestFormatTaskStatus_AllDeclaredPresent(t *testing.T) {
	root := t.TempDir()
	writeDeclaredArtifact(t, root, "scheduling_plan.json", "{}")
	writeDeclaredArtifact(t, root, "customer_reply.md", "hi")

	out := formatTaskStatus("task_1", map[string]any{
		"status":         "completed",
		"workspace_root": root,
	}, []string{"scheduling_plan.json", "customer_reply.md"})

	if !strings.Contains(out, "declared 2 artifact(s); present 2") {
		t.Fatalf("want present 2, got:\n%s", out)
	}
	if strings.Contains(out, "missing:") {
		t.Errorf("nothing is missing, so the word must not appear, got:\n%s", out)
	}
	if !strings.Contains(out, "read them and answer") {
		t.Errorf("a complete task should say to read the artifacts, got:\n%s", out)
	}
}

// The strict-landing test: finding the file under <root>/workspace/ is not
// "present". Softening this into a recursive search would turn the silent
// misplacement this whole change exists for into a green light (→172).
func TestFormatTaskStatus_NestedCopyCountsAsMissing(t *testing.T) {
	root := t.TempDir()
	writeDeclaredArtifact(t, root, filepath.ToSlash(filepath.Join("workspace", "customer_reply.md")), "hi")

	out := formatTaskStatus("task_1", map[string]any{
		"status":         "completed",
		"workspace_root": root,
	}, []string{"customer_reply.md"})

	if !strings.Contains(out, "present 0") || !strings.Contains(out, "missing: customer_reply.md") {
		t.Fatalf("a file in the nested workspace copy must count as missing, got:\n%s", out)
	}
}

// Legacy path: no declared list means the previous wording is preserved, plus one
// sentence that makes "nothing was declared" distinguishable from "declared and
// not delivered".
func TestFormatTaskStatus_NoDeclaredListKeepsLegacyWording(t *testing.T) {
	root := t.TempDir()
	out := formatTaskStatus("task_1", map[string]any{
		"status":         "completed",
		"workspace_root": root,
	}, nil)

	if !strings.Contains(out, "declared no output files") {
		t.Fatalf("legacy wording must survive, got:\n%s", out)
	}
	if !strings.Contains(out, "no artifacts were declared for this task") {
		t.Errorf("the receipt must distinguish 'not declared' from 'not delivered', got:\n%s", out)
	}
	if strings.Contains(out, "declared 0 artifact(s)") {
		t.Errorf("a nil list is not a contract of zero artifacts, got:\n%s", out)
	}
}
