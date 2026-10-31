package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the contract that externalizeArtifact depends on: the pointer
// it hands back must be readable by the very tool it tells the model to use.
//
// Found on harness-bench 026 (v60, 2026-10-30). The delegated sub-agent wrote a
// 6 KB review_findings.json; the parent read it back and got
//
//	[Artifact externalized: 6 KB. Skeleton below. Full content at:
//	 /state/outputs/chat_hb-.../artifacts/art_..._read_file.txt]
//	[JSON Skeleton: 6 KB, 76 lines]  L14: "id": string  L15: "severity": string ...
//	[Use read_file to access full content.]
//
// i.e. the model was told to read_file a path, after being shown a skeleton that
// contains field NAMES and no values. Both halves below are broken.

// artifactPathOf pulls the path out of the externalization pointer.
func artifactPathOf(t *testing.T, result string) string {
	t.Helper()
	const marker = "Full content at: "
	i := strings.Index(result, marker)
	if i < 0 {
		t.Fatalf("externalized result carries no pointer: %s", truncateForTest(result, 200))
	}
	rest := result[i+len(marker):]
	j := strings.Index(rest, "]")
	if j < 0 {
		t.Fatalf("pointer is not terminated: %s", truncateForTest(rest, 200))
	}
	return rest[:j]
}

func truncateForTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestExternalizedArtifact_IsReadableByReadFile is the load-bearing half: the
// prompt says "[Use read_file to access full content.]", so read_file must be
// able to open that absolute path.
func TestExternalizedArtifact_IsReadableByReadFile(t *testing.T) {
	sessionRoot := t.TempDir() // stands in for /workspace
	outputBase := t.TempDir()  // stands in for /state/outputs (outside the workspace)
	h := &ChatHarness{OutputBase: outputBase, SessionRoot: sessionRoot}
	ctx := context.Background()
	const taskID = "task-recall"

	big := `{"findings":[{"id":"F-1","severity":"high","risk":"bulk export skips the tenant filter"}],` +
		`"notes":"` + strings.Repeat("padding ", 400) + `"}`
	if len(big) <= artifactThreshold {
		t.Fatalf("fixture must exceed the %d byte threshold, got %d", artifactThreshold, len(big))
	}
	if err := os.WriteFile(filepath.Join(sessionRoot, "findings.json"), []byte(big), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	res, err := h.execTool(ctx, "read_file", map[string]any{"path": "findings.json"}, taskID)
	if err != nil {
		t.Fatalf("first read failed: %v", err)
	}
	if !strings.Contains(res, "bulk export") {
		t.Fatalf("read_file did not return the content in the first place: %s", truncateForTest(res, 200))
	}

	ext := h.externalizeArtifact(res, "read_file", taskID)
	ptr := artifactPathOf(t, ext)

	back, err := h.execTool(ctx, "read_file", map[string]any{"path": ptr}, taskID)
	if err != nil {
		t.Fatalf("the pointer handed to the model is unreadable (%v); path=%s", err, ptr)
	}
	if !strings.Contains(back, "bulk export") {
		t.Fatalf("reading the artifact back did not return the content: %s", truncateForTest(back, 200))
	}
}

// TestShouldExternalize_ExemptsReadFile pins the second half: a read_file result
// must not be replaced by a skeleton, because read_file IS the recall path the
// pointer points at. Externalizing it makes the loop non-terminating — every
// read produces a new artifact and a new skeleton, so no tool result over the
// threshold is ever visible to the model.
func TestShouldExternalize_ExemptsReadFile(t *testing.T) {
	h := &ChatHarness{OutputBase: t.TempDir(), SessionRoot: t.TempDir()}
	big := strings.Repeat("x", artifactThreshold+1)
	small := strings.Repeat("x", artifactThreshold-1)

	cases := []struct {
		tool   string
		result string
		want   bool
	}{
		{"execute_code", big, true},
		{"write_file", big, true},
		{"read_file", big, false}, // content the model asked for, verbatim
		{"execute_code", small, false},
		{"read_file", small, false},
	}
	for _, c := range cases {
		if got := h.shouldExternalize(c.tool, c.result); got != c.want {
			t.Errorf("shouldExternalize(%q, %d bytes) = %v, want %v", c.tool, len(c.result), got, c.want)
		}
	}

	// A read larger than the ceiling is externalized — read_file has no output
	// cap, so passing a 5 MB read through would flood the window.
	huge := strings.Repeat("x", readPassthroughCeiling+1)
	if !h.shouldExternalize("read_file", huge) {
		t.Errorf("read_file above the %d byte ceiling must still be externalized", readPassthroughCeiling)
	}

	// No output base means no place to put the artifact — never externalize.
	bare := &ChatHarness{}
	if bare.shouldExternalize("execute_code", big) {
		t.Error("shouldExternalize must be false when OutputBase is unset")
	}
}
