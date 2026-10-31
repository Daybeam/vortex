package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// mockToolLoopProvider plays a fixed script: for the first `rounds` provider
// calls it returns the tool calls produced by `mk(n)`, then it answers. Each
// round uses a different payload so the signature-based loop guards stay quiet —
// which is the whole reason the write guard had to be added.
type mockToolLoopProvider struct {
	rounds int
	mk     func(n int) []schemas.ToolCall
	calls  int
}

func (m *mockToolLoopProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *mockToolLoopProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	if m.calls <= m.rounds {
		return &schemas.ProviderResponse{ToolCalls: m.mk(m.calls)}, nil
	}
	onChunk("done")
	return &schemas.ProviderResponse{Text: "done"}, nil
}

func (m *mockToolLoopProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *mockToolLoopProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *mockToolLoopProvider) Name() string { return "mock-tool-loop" }

// runWithProvider drives one chat turn and returns every emitted event.
func runWithProvider(t *testing.T, p *mockToolLoopProvider, sessionRoot string) []ChatEvent {
	t.Helper()
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: sessionRoot,
		MaxTurns:    12,
	}
	var events []ChatEvent
	_, _ = h.Run(context.Background(), "task-write-guard",
		[]ChatMessage{{Role: "user", Content: "write the report"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })
	return events
}

func toolResultsFor(events []ChatEvent, tool string) []string {
	var out []string
	for _, ev := range events {
		if ev.Type != "tool_result" {
			continue
		}
		if t, _ := ev.Meta["tool"].(string); t == tool {
			out = append(out, ev.Data)
		}
	}
	return out
}

func repeatedWriteEvents(events []ChatEvent) []ChatEvent {
	var out []ChatEvent
	for _, ev := range events {
		if ev.Type == "repeated_write" {
			out = append(out, ev)
		}
	}
	return out
}

// TestWriteGuard_SteersOnThirdRewriteOfSamePath is the D7 acceptance test: the
// 026 shape — N successive write_file calls to one path, each with different
// content. Writes 1 and 2 must be left alone (write-then-fix is normal work);
// from write 3 on, the tool result must carry the steer.
func TestWriteGuard_SteersOnThirdRewriteOfSamePath(t *testing.T) {
	const target = "review_findings.json"
	p := &mockToolLoopProvider{
		rounds: 5,
		mk: func(n int) []schemas.ToolCall {
			return []schemas.ToolCall{{
				Name:      "write_file",
				Arguments: map[string]any{"path": target, "content": fmt.Sprintf(`{"version": %d}`, n)},
			}}
		},
	}
	events := runWithProvider(t, p, "")

	results := toolResultsFor(events, "write_file")
	if len(results) < 5 {
		t.Fatalf("expected at least 5 write_file results, got %d", len(results))
	}
	// A host that memory-maps a freshly written file (Windows AV does) turns a
	// successful write into an error, which puts the result on the failure path
	// where the write guard deliberately does not count. Report that as "not
	// measured" instead of a red, so the next reader does not chase a guard
	// regression that does not exist.
	for i, r := range results {
		if strings.Contains(r, "[TOOL FAILED") || strings.Contains(r, "HAS FAILED") {
			t.Skipf("host refused write #%d (%s); write guard not measurable on this host", i+1, r)
		}
	}
	for i := 0; i < 2; i++ {
		if strings.Contains(results[i], "REPEATED WRITE") {
			t.Errorf("write #%d must not be steered (threshold is %d): %s", i+1, repeatedWriteThreshold, results[i])
		}
	}
	for i := 2; i < 5; i++ {
		if !strings.Contains(results[i], "REPEATED WRITE") {
			t.Errorf("write #%d must carry the steer, got: %s", i+1, results[i])
		}
		if !strings.Contains(results[i], "review_findings.json") {
			t.Errorf("write #%d steer must name the target path, got: %s", i+1, results[i])
		}
	}

	// The steer also has to be observable outside the model: T1 counting works
	// off these events, and a silent guard is a guard nobody can measure.
	evs := repeatedWriteEvents(events)
	if len(evs) != 3 {
		t.Fatalf("expected 3 repeated_write events (writes 3..5), got %d", len(evs))
	}
	if n, _ := evs[0].Meta["count"].(int); n != 3 {
		t.Errorf("first repeated_write event must report count=3, got %v", evs[0].Meta["count"])
	}
	if evs[0].Data != target {
		t.Errorf("event should carry the target path, got %q", evs[0].Data)
	}
}

// TestWriteGuardSemantics_ExactSequence pins the counting contract itself,
// without a filesystem: five writes to one target, only the last three steered,
// and the steer names the write index. This is the test that must stay green on
// every host.
func TestWriteGuardSemantics_ExactSequence(t *testing.T) {
	counts := make(map[string]int)
	want := []struct {
		index int
		steer bool
	}{
		{1, false}, {2, false}, {3, true}, {4, true}, {5, true},
	}
	for i, w := range want {
		n, steer := writeGuardSteer(counts, "write_file", "review_findings.json")
		if n != w.index {
			t.Fatalf("write #%d reported index %d", i+1, n)
		}
		if (steer != "") != w.steer {
			t.Fatalf("write #%d: steer presence = %v, want %v (text: %q)", i+1, steer != "", w.steer, steer)
		}
		if w.steer && !strings.Contains(steer, fmt.Sprintf("#%d", w.index)) {
			t.Errorf("write #%d steer must carry the write index, got: %s", i+1, steer)
		}
	}
	// A different target on the same tool starts its own count.
	if n, steer := writeGuardSteer(counts, "write_file", "other.json"); n != 1 || steer != "" {
		t.Errorf("new target must start at 1 with no steer, got n=%d steer=%q", n, steer)
	}
	// A different tool writing the same target also starts its own count.
	if n, steer := writeGuardSteer(counts, "append_file", "review_findings.json"); n != 1 || steer != "" {
		t.Errorf("new tool must start at 1 with no steer, got n=%d steer=%q", n, steer)
	}
	// Nil map / empty target are no-ops rather than panics.
	if n, steer := writeGuardSteer(nil, "write_file", "x"); n != 0 || steer != "" {
		t.Errorf("nil counts must be a no-op, got n=%d steer=%q", n, steer)
	}
	if n, steer := writeGuardSteer(counts, "write_file", ""); n != 0 || steer != "" {
		t.Errorf("empty target must be a no-op, got n=%d steer=%q", n, steer)
	}
}

// TestWriteGuard_DifferentPathsDoNotSteer guards the false-positive side: a
// multi-file task writes several files in a row and must never be interrupted.
func TestWriteGuard_DifferentPathsDoNotSteer(t *testing.T) {
	paths := []string{"a.md", "b.md", "c.md", "d.md", "e.md"}
	p := &mockToolLoopProvider{
		rounds: len(paths),
		mk: func(n int) []schemas.ToolCall {
			return []schemas.ToolCall{{
				Name:      "write_file",
				Arguments: map[string]any{"path": paths[n-1], "content": "x"},
			}}
		},
	}
	events := runWithProvider(t, p, "")

	for i, r := range toolResultsFor(events, "write_file") {
		if strings.Contains(r, "REPEATED WRITE") {
			t.Errorf("distinct-path write #%d must not be steered, got: %s", i+1, r)
		}
	}
	if evs := repeatedWriteEvents(events); len(evs) != 0 {
		t.Errorf("expected no repeated_write events for distinct paths, got %d", len(evs))
	}
}

// TestWriteGuard_MixedSpellingsAreOneTarget is the normalization half of the
// acceptance test: 026 mixed spellings of the same file, so a key built from the
// raw argument would have counted them separately and the guard would never
// have fired. Three textually different paths, one real file, must trip the
// guard on the third write.
func TestWriteGuard_MixedSpellingsAreOneTarget(t *testing.T) {
	root := t.TempDir()
	spellings := []string{"review_findings.json", "./review_findings.json", "sub/../review_findings.json"}
	p := &mockToolLoopProvider{
		rounds: len(spellings),
		mk: func(n int) []schemas.ToolCall {
			return []schemas.ToolCall{{
				Name:      "write_file",
				Arguments: map[string]any{"path": spellings[n-1], "content": fmt.Sprintf(`{"v": %d}`, n)},
			}}
		},
	}
	events := runWithProvider(t, p, root)

	results := toolResultsFor(events, "write_file")
	if len(results) != 3 {
		t.Fatalf("expected 3 write results, got %d", len(results))
	}
	if !strings.Contains(results[2], "REPEATED WRITE") {
		t.Fatalf("third write to the same file under a different spelling must be steered, got: %s", results[2])
	}
	if evs := repeatedWriteEvents(events); len(evs) != 1 {
		t.Fatalf("expected exactly 1 repeated_write event, got %d", len(evs))
	}
}

// TestWriteGuard_ReadToolsUnaffected pins that the guard did not leak into the
// read path: three identical read_file calls must produce zero repeated_write
// events, because reads are already covered by cache replay / result repetition.
func TestWriteGuard_ReadToolsUnaffected(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	p := &mockToolLoopProvider{
		rounds: 3,
		mk: func(n int) []schemas.ToolCall {
			return []schemas.ToolCall{{
				Name:      "read_file",
				Arguments: map[string]any{"path": "notes.txt", "start_line": float64(n)},
			}}
		},
	}
	events := runWithProvider(t, p, root)

	if evs := repeatedWriteEvents(events); len(evs) != 0 {
		t.Errorf("read_file must never trigger the write guard, got %d events", len(evs))
	}
	for i, r := range toolResultsFor(events, "read_file") {
		if strings.Contains(r, "REPEATED WRITE") {
			t.Errorf("read #%d must not be steered, got: %s", i+1, r)
		}
	}
}

// TestWriteGuard_CountResetsPerTurn pins the scope of the counter. Two files
// written once per turn, across two turns, is not a rewrite loop — the count
// lives in the turn's run() scope and must not leak between turns.
func TestWriteGuard_CountResetsPerTurn(t *testing.T) {
	p := &mockToolLoopProvider{
		rounds: 2,
		mk: func(n int) []schemas.ToolCall {
			return []schemas.ToolCall{{
				Name:      "write_file",
				Arguments: map[string]any{"path": "report.md", "content": fmt.Sprintf("v%d", n)},
			}}
		},
	}
	for turn := 0; turn < 2; turn++ {
		events := runWithProvider(t, p, "")
		if evs := repeatedWriteEvents(events); len(evs) != 0 {
			t.Errorf("turn %d: 2 writes must not trip the guard, got %d events", turn+1, len(evs))
		}
	}
}

// TestWriteGuard_ThresholdIsAtLeastThree keeps the guard from being tightened to
// 2 by a future edit: write-then-fix is a legitimate two-write pattern.
func TestWriteGuard_ThresholdIsAtLeastThree(t *testing.T) {
	if repeatedWriteThreshold < 3 {
		t.Fatalf("repeatedWriteThreshold must stay >= 3 (write-then-fix is normal), got %d", repeatedWriteThreshold)
	}
}

// ── unit tests for the key derivation ────────────────────────────────

func TestNormalizeWriteTarget(t *testing.T) {
	cases := []struct {
		in, root, want string
	}{
		{"review_findings.json", "", "review_findings.json"},
		{"./review_findings.json", "", "review_findings.json"},
		{"a/../review_findings.json", "", "review_findings.json"},
		{"review_findings.json", "/workspace", "review_findings.json"},
		{"/workspace/review_findings.json", "/workspace", "review_findings.json"},
		{"/workspace/./review_findings.json", "/workspace/", "review_findings.json"},
		{"/workspace", "/workspace", ""},
		{"  review_findings.json  ", "", "review_findings.json"},
		{``, "", ""},
		{`outputs\task\review_findings.json`, "", "outputs/task/review_findings.json"},
	}
	for _, c := range cases {
		if got := normalizeWriteTarget(c.in, c.root); got != c.want {
			t.Errorf("normalizeWriteTarget(%q, %q) = %q, want %q", c.in, c.root, got, c.want)
		}
	}
}

func TestWriteTargetOf_SkipsNonFileWrites(t *testing.T) {
	h := &ChatHarness{SessionRoot: "/workspace"}
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"write_file", map[string]any{"path": "a.json"}, "a.json"},
		{"write_file", map[string]any{"path": "/workspace/a.json"}, "a.json"},
		{"write_file", map[string]any{"content": "no path"}, ""},
		{"write_file", nil, ""},
		{"read_file", map[string]any{"path": "a.json"}, ""},
		{"execute_code", map[string]any{"code": "open('a.json','w')"}, ""},
		{"orchestrator_task_status", map[string]any{"task_id": "t1"}, ""},
		{"delegate_to_orchestrator", map[string]any{"prompt": "do it"}, ""},
	}
	for _, c := range cases {
		if got := h.writeTargetOf(c.tool, c.args); got != c.want {
			t.Errorf("writeTargetOf(%q, %v) = %q, want %q", c.tool, c.args, got, c.want)
		}
	}
}
