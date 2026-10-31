package skeleton

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Regression tests for the jsonSkeleton panic of 2026-10-30.
//
// Observed in the field (harness-bench run p3-d3d5, task 026):
//
//	panic: runtime error: index out of range [-1]
//	  core/skeleton.jsonSkeleton        file_skeleton.go:142
//	  core/skeleton.DetectAndSkeletonize detect.go:32
//	  core.ChatHarness.externalizeArtifact
//
// The input was a 5804-byte, fully valid JSON read_file result. Root cause:
// the tree structure was tracked in three parallel slices (ctx, arrayCount,
// arrayStartLine). The "skip output beyond the first array element" path
// adjusted `depth` only, while the opener of that first element had already
// been pushed by the printing path — so `ctx` kept a permanent extra frame.
// When the closing ']' later popped that stale frame instead of the array
// frame, `ctx` still claimed "inside an array" while `arrayCount` was empty,
// and the next string indexed arrayCount[-1].
//
// The fix replaces the parallel slices with one []jsonFrame, keeps the stack
// in lockstep on both paths, and pops only a frame the closer actually
// matches. These tests pin the trigger *shape*, not just the one payload:
// an array whose first element is a container, followed by any sibling token.

// TestJSONSkeleton_ContainerInArrayDoesNotPanic covers the minimal shapes.
// Every case is valid JSON; none may panic.
func TestJSONSkeleton_ContainerInArrayDoesNotPanic(t *testing.T) {
	cases := []struct{ name, in string }{
		// The exact minimal reproducer: {"a":[{...}],"b":...}
		{"object-in-array-then-key", `{"a":[{"x":"y"}],"b":"s"}`},
		{"two-objects-in-array-then-key", `{"a":[{"x":"y"},{"z":"w"}],"b":"s"}`},
		{"deeper-nesting", `{"a":[{"b":[{"c":1}],"d":"e"}],"f":"g"}`},
		{"object-in-array-only", `{"a":[{"x":"y"}]}`},
		{"root-array-of-objects", `[{"x":"y"},{"z":"w"}]`},
		{"array-of-strings-then-key", `{"a":["p","q"],"b":"s"}`},
		{"array-of-numbers-then-key", `{"a":[1,2],"b":"s"}`},
		{"array-of-arrays-then-key", `{"a":[[1],[2]],"b":"s"}`},
		{"empty-array-then-key", `{"a":[],"b":"s"}`},
		{"array-of-null-and-bool", `{"a":[null,true,false],"b":"s"}`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on valid JSON %s: %v", c.in, r)
				}
			}()
			out := jsonSkeleton([]byte(c.in))
			if !strings.Contains(out, "[JSON Skeleton:") {
				t.Fatalf("missing header for %s, got %q", c.in, out)
			}
		})
	}
}

// TestJSONSkeleton_NestingMatrix walks a systematic matrix of container
// combinations. The bug was a *shape* bug, so a hand-picked list can miss a
// neighbouring shape; this generates the permutations instead.
func TestJSONSkeleton_NestingMatrix(t *testing.T) {
	elem := map[string]string{
		"object": `{"k":"v"}`,
		"array":  `["v"]`,
		"scalar": `"v"`,
		"num":    `7`,
	}
	tails := map[string]string{
		"key":    `,"t":"s"`,
		"none":   ``,
		"object": `,"t":{"z":1}`,
	}
	for en, ev := range elem {
		for tn, tv := range tails {
			for _, wrap := range []string{
				`{"a":[%s]%s}`,
				`{"a":[%s,%s]%s}`,
				`[%s]%s`,
			} {
				var in string
				if strings.Count(wrap, "%s") == 3 {
					in = fmt.Sprintf(wrap, ev, ev, tv)
				} else {
					in = fmt.Sprintf(wrap, ev, tv)
				}
				name := fmt.Sprintf("%s_%s_%d", en, tn, strings.Count(in, "["))
				payload := in
				t.Run(name, func(t *testing.T) {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panic on %s: %v", payload, r)
						}
					}()
					if out := jsonSkeleton([]byte(payload)); !strings.Contains(out, "[JSON Skeleton:") {
						t.Fatalf("missing header for %s", payload)
					}
				})
			}
		}
	}
}

// TestJSONSkeleton_StringArrayCountStillReported guards the other direction:
// the stack rewrite must not make the array summary disappear. Without this,
// "does not panic" could be satisfied by skipping everything.
func TestJSONSkeleton_StringArrayCountStillReported(t *testing.T) {
	out := jsonSkeleton([]byte(`{"items":["a","b","c"],"count":3}`))
	if !strings.Contains(out, "[array: 3 elements]") {
		t.Fatalf("expected array element count in skeleton, got:\n%s", out)
	}
	if !strings.Contains(out, `"items"`) {
		t.Fatalf("expected array key in skeleton, got:\n%s", out)
	}
}

// TestJSONSkeleton_RealReadFilePayload replays the exact bytes that crashed
// the 026 run. Kept as testdata so the shape cannot silently regress.
func TestJSONSkeleton_RealReadFilePayload(t *testing.T) {
	data, err := os.ReadFile("testdata/review_findings_5804.json")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	if len(data) != 5804 {
		t.Fatalf("testdata changed size: %d, want 5804", len(data))
	}

	var out string
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on real payload: %v", r)
			}
		}()
		out = DetectAndSkeletonize(data, "read_file")
	}()

	if !strings.Contains(out, "[JSON Skeleton:") {
		t.Fatalf("real payload not routed to jsonSkeleton, got:\n%s", out)
	}
	if !strings.Contains(out, `"findings"`) {
		t.Fatalf("skeleton lost the top-level findings key, got:\n%s", out)
	}
	// The whole point of externalization is shrinking the context payload.
	if len(out) >= len(data) {
		t.Fatalf("skeleton did not shrink payload: %d >= %d", len(out), len(data))
	}
}

// TestFileSkeleton_JSONArrayOfObjects is the same shape through the
// extension-based entry point, which shares jsonSkeleton.
func TestFileSkeleton_JSONArrayOfObjects(t *testing.T) {
	in := []byte(`{"rows":[{"a":1},{"b":2}],"total":2}`)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("FileSkeleton panicked: %v", r)
		}
	}()
	if out := FileSkeleton(in, "rows.json"); !strings.Contains(out, "JSON Skeleton") {
		t.Fatalf("expected JSON skeleton, got %q", out)
	}
}
