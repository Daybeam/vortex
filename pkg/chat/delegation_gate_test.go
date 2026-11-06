package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// Delegation gate (Stage 3, docs/plan/COST_AND_TERMINATION_PLAN.md 阶段 3,
// commit 49e9ca3ea): when every artifact the task prompt declared already
// exists, delegate_to_orchestrator returns a skip receipt instead of starting
// a run. The gate is pure cost avoidance — a delegated task re-does work the
// workspace already holds — so the cases that matter are the two directions it
// can be wrong in: firing when work is still missing, or never firing at all.
//
// gateProvider records, for every provider call, how many times the skip
// receipt is present in the prompt the model receives. Same witness as
// contractProvider in contract_steer_test.go: the tool result says the harness
// produced a receipt, the request proves the model could read it. A receipt the
// model never sees steers nothing.
type gateProvider struct {
	turn     int
	skipSeen []int
	script   []func() *schemas.ProviderResponse
}

func (p *gateProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *gateProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.skipSeen = append(p.skipSeen, strings.Count(req.User, core.MarkerDelegationSkipped))
	idx := p.turn
	p.turn++
	if idx < len(p.script) {
		return p.script[idx](), nil
	}
	return &schemas.ProviderResponse{Text: "done"}, nil
}

func (p *gateProvider) Embed(ctx context.Context, text string) ([]float32, error) { return nil, nil }
func (p *gateProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (p *gateProvider) Name() string                                              { return "gate-mock" }

func delegateCall(prompt string) func() *schemas.ProviderResponse {
	return func() *schemas.ProviderResponse {
		return &schemas.ProviderResponse{
			Text: "handing this off",
			ToolCalls: []schemas.ToolCall{
				{Name: "delegate_to_orchestrator", Arguments: map[string]any{"prompt": prompt}},
			},
		}
	}
}

// delegateSpy is the cost the gate is supposed to avoid paying: every call is a
// background run of its own.
type delegateSpy struct {
	prompts []string
}

func (s *delegateSpy) delegate(prompt string) (string, error) {
	s.prompts = append(s.prompts, prompt)
	return "task_gate0", nil
}

func (s *delegateSpy) calls() int { return len(s.prompts) }

// plant creates declared artifacts in dir, so "already exists on disk" is a
// fact about the workspace rather than an accident of an empty one.
func plant(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("pre-existing\n"), 0644); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
	}
}

// The gate reads disk on every call, so one run can legitimately need both
// answers. This is the load-bearing scenario, in both directions at once:
// delegating while work is missing, then refusing to delegate after the last
// artifact lands.
//
// The second delegate call uses a different prompt on purpose. Identical
// arguments would let cache replay or a loop guard answer first, and the test
// would report the gate working while the code under test was never reached.
func TestDelegationGate_FiresOnlyAfterTheLastArtifactLands(t *testing.T) {
	sessionRoot := t.TempDir()
	plant(t, sessionRoot, "a.csv") // b.json is still owed

	p := &gateProvider{script: []func() *schemas.ProviderResponse{
		delegateCall("reconcile the invoices"), // missing b.json: must delegate
		writeArtifact("b.json"),                // contract now satisfied
		delegateCall("reconcile the rest"),     // identical work: must be skipped
		stopWithText(),
	}}
	spy := &delegateSpy{}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: sessionRoot,
		Delegate:    spy.delegate,
		MaxTurns:    6,
	}

	var events []ChatEvent
	if _, err := h.Run(context.Background(), "task-gate",
		[]ChatMessage{{Role: "user", Content: contractPrompt}},
		func(ev ChatEvent) error { events = append(events, ev); return nil }); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if spy.calls() != 1 {
		t.Fatalf("delegate called %d times, want exactly 1 (prompts %q)", spy.calls(), spy.prompts)
	}

	// call 0: nothing delegated yet. call 1: sees the delegate receipt.
	// call 2: sees the write result. call 3: sees the skip receipt.
	want := []int{0, 0, 0, 1}
	if len(p.skipSeen) != len(want) {
		t.Fatalf("provider called %d times (%v), want %d", len(p.skipSeen), p.skipSeen, len(want))
	}
	for i := range want {
		if p.skipSeen[i] != want[i] {
			t.Fatalf("skip-receipt count per provider call = %v, want %v", p.skipSeen, want)
		}
	}

	// The gate answers with a receipt, never an error: an error would consume a
	// retry turn and invite the model to reach for a different tool instead.
	if n := countEvents(events, "tool_error"); n != 0 {
		t.Fatalf("tool_error events = %d, want 0 (the gate must not look like a failure)", n)
	}

	var receipts []string
	for _, ev := range events {
		if ev.Type == "tool_result" && ev.Meta["tool"] == "delegate_to_orchestrator" {
			receipts = append(receipts, ev.Data)
		}
	}
	if len(receipts) != 2 {
		t.Fatalf("delegate tool results = %d, want 2 (one delegated, one skipped)", len(receipts))
	}
	if !strings.HasPrefix(receipts[0], "delegated task_id=task_gate0") {
		t.Errorf("first delegate call went through the normal path, got:\n%s", receipts[0])
	}
	// The receipt leads the message so the model reads "skipped" before any
	// steer appended after it.
	if !strings.HasPrefix(receipts[1], core.MarkerDelegationSkipped) {
		t.Errorf("second delegate call must open with the skip receipt, got:\n%s", receipts[1])
	}
}

// The gate is read from h.declaredArtifacts, which Run populates from the task
// prompt. Set by hand here: execTool is unexported and this file lives in the
// package, so the branch matrix is tested without re-driving the whole loop.
// The wiring — prompt to field — is covered by the run above, which never
// touches the field.
func TestDelegationGate_BranchMatrix(t *testing.T) {
	declared := []string{"a.csv", "b.json"}
	// A workspace holding exactly what the contract declares, so the only thing
	// that can make the gate fire is the gate's own two preconditions.
	rootWithBoth := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		plant(t, root, "a.csv", "b.json")
		return root
	}

	tests := []struct {
		name       string
		session    func(t *testing.T) string
		declared   []string
		wantMarker bool
		why        string
	}{
		{
			name:       "all present",
			session:    rootWithBoth,
			declared:   declared,
			wantMarker: true,
			why:        "the work the contract promises is already on disk",
		},
		{
			name:     "one missing",
			session:  rootWithBoth,
			declared: []string{"a.csv", "b.json", "c.parquet"}, // c.parquet is never planted
			why:      "an unfinished contract must not be told to stop",
		},
		{
			name:    "no contract",
			session: rootWithBoth,
			why: "nil means 'this prompt declares nothing'. Without the length " +
				"check WorkspaceArtifactsComplete returns missing=0 for an empty " +
				"list, so every ordinary chat would be told to stop delegating.",
		},
		{
			name:     "legacy layout",
			session:  func(t *testing.T) string { return "" },
			declared: declared,
			why: "with no workspace root the declared names resolve against the " +
				"process working directory, which is the deploy directory, not " +
				"this run's output",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "legacy layout" {
				// The trap: identical names present in the working directory.
				// Absent them the test passes for the wrong reason — nothing is
				// found anywhere — and deleting the SessionRoot check would not
				// turn it red.
				cwd := t.TempDir()
				plant(t, cwd, "a.csv", "b.json")
				t.Chdir(cwd)
			}
			h := &ChatHarness{
				SessionRoot: tc.session(t),
				Delegate:    func(prompt string) (string, error) { return "task_gate0", nil },
			}
			h.declaredArtifacts = tc.declared

			out, err := h.execTool(context.Background(), "delegate_to_orchestrator",
				map[string]any{"prompt": "reconcile the invoices"}, "chat_gate")
			if err != nil {
				t.Fatalf("execTool returned an error, want a receipt (%s): %v", tc.why, err)
			}
			got := strings.HasPrefix(out, core.MarkerDelegationSkipped)
			if got != tc.wantMarker {
				t.Fatalf("gate fired=%v, want %v (%s), got:\n%s", got, tc.wantMarker, tc.why, out)
			}
			if tc.wantMarker && out != core.MarkerDelegationSkipped {
				t.Errorf("the gate must answer with the marker itself, got:\n%s", out)
			}
			if !tc.wantMarker && !strings.HasPrefix(out, "delegated task_id=task_gate0") {
				t.Errorf("delegation was skipped: %s", tc.why)
			}
		})
	}
}
