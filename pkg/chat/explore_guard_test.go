package chat

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// ── P1 exploration guard: streak membership, reset, escalation ──────────

func newExploreGuard() *exploreGuard {
	return &exploreGuard{writeTargets: make(map[string]struct{}), writeCounts: make(map[string]int)}
}

func exploreCall(t *testing.T, h *ChatHarness, g *exploreGuard, mode codeExecGuardMode, limit int,
	tool string, events *[]string) (toolAfterRule, string) {
	t.Helper()
	var result string
	rule := h.afterToolCall(
		schemas.ToolCall{Name: tool, Arguments: map[string]any{}},
		&result, g, mode, limit, nil,
		func(ev ChatEvent) error { *events = append(*events, ev.Type); return nil })
	return rule, result
}

// The default arm must stay advisory: one steer at the threshold, then silence,
// and the newly-covered search_archive calls count toward it.
func TestExploreGuardDefaultSteersOnceAndCountsSearchArchive(t *testing.T) {
	h := &ChatHarness{}
	g := newExploreGuard()
	var events []string

	for i := 1; i <= 5; i++ {
		rule, out := exploreCall(t, h, g, codeExecGuardSteer, 6, "search_archive", &events)
		if rule != (toolAfterRule{}) {
			t.Fatalf("call %d: default arm must never stop anything, got %+v", i, rule)
		}
		if strings.Contains(out, "[NO NEW FILES]") {
			t.Fatalf("call %d: steer fired before the threshold:\n%s", i, out)
		}
	}
	_, out := exploreCall(t, h, g, codeExecGuardSteer, 6, "search_archive", &events)
	if !strings.Contains(out, "[NO NEW FILES]") || !strings.Contains(out, "6 consecutive") {
		t.Errorf("the sixth consecutive search_archive call must be steered:\n%s", out)
	}
	if g.streak != 6 || g.streakMax != 6 || g.steers != 1 {
		t.Errorf("streak=%d max=%d steers=%d, want 6/6/1", g.streak, g.streakMax, g.steers)
	}

	// Latched: the rest of the run stays silent (pre-P1-1 behaviour).
	for i := 0; i < 3; i++ {
		_, out = exploreCall(t, h, g, codeExecGuardSteer, 6, "execute_code", &events)
		if strings.Contains(out, "[NO NEW FILES]") {
			t.Errorf("the default correction is one per run, fired again at streak %d:\n%s", g.streak, out)
		}
	}
	if g.steers != 1 || g.streak != 9 {
		t.Errorf("steers=%d streak=%d, want 1/9", g.steers, g.streak)
	}
}

// A successful write is the evidence the guard is waiting for: it resets the
// streak, so post-write exploration is measured from zero.
func TestExploreGuardWriteResetsStreak(t *testing.T) {
	h := &ChatHarness{}
	g := newExploreGuard()
	var events []string

	for i := 0; i < 4; i++ {
		exploreCall(t, h, g, codeExecGuardSteer, 6, "execute_code", &events)
	}
	var out string
	h.afterToolCall(schemas.ToolCall{Name: "write_file", Arguments: map[string]any{"path": "report.md"}},
		&out, g, codeExecGuardSteer, 6, nil, func(ChatEvent) error { return nil })
	if g.streak != 0 || g.writeTotal != 1 {
		t.Fatalf("write must reset the streak, got streak=%d writes=%d", g.streak, g.writeTotal)
	}
	if _, ok := g.writeTargets["report.md"]; !ok {
		t.Errorf("write target not recorded: %v", g.writeTargets)
	}
}

// Off must stay inert: no streak, no events, nothing to stop.
func TestExploreGuardOffIsInert(t *testing.T) {
	h := &ChatHarness{}
	g := newExploreGuard()
	var events []string
	for i := 0; i < 30; i++ {
		rule, out := exploreCall(t, h, g, codeExecGuardOff, 6, "execute_code", &events)
		if rule != (toolAfterRule{}) || out != "" {
			t.Fatalf("off arm changed a result at call %d: rule=%+v out=%q", i+1, rule, out)
		}
	}
	if g.streak != 0 || len(events) != 0 {
		t.Errorf("off arm counted streak=%d events=%v", g.streak, events)
	}
}

// escalate is the opt-in hard arm: 6 steers, 12 stops the batch, 24 ends the
// run. Each level fires exactly once and in order.
func TestExploreGuardEscalateLevels(t *testing.T) {
	h := &ChatHarness{}
	g := newExploreGuard()
	var events []string
	const limit = 6

	for i := 1; i <= 24; i++ {
		rule, out := exploreCall(t, h, g, codeExecGuardEscalate, limit, "execute_code", &events)
		switch {
		case i < limit:
			if rule != (toolAfterRule{}) || g.level != 0 {
				t.Fatalf("call %d: escalated before the threshold: rule=%+v level=%d", i, rule, g.level)
			}
		case i == limit:
			if g.level != 1 || rule != (toolAfterRule{}) || !strings.Contains(out, "[NO NEW FILES]") {
				t.Fatalf("call %d: want level 1 steer only, got level=%d rule=%+v out=%q", i, g.level, rule, out)
			}
		case i == 2*limit:
			if g.level != 2 || !rule.stopBatch || rule.endRun {
				t.Fatalf("call %d: want stopBatch, got level=%d rule=%+v", i, g.level, rule)
			}
			if !strings.Contains(out, "ESCALATION") {
				t.Errorf("level 2 must say what is happening:\n%s", out)
			}
		case i == 4*limit:
			if g.level != 3 || !rule.endRun {
				t.Fatalf("call %d: want endRun, got level=%d rule=%+v", i, g.level, rule)
			}
			if !strings.Contains(out, "EXPLORATION CEILING") {
				t.Errorf("level 3 must say the run is ending:\n%s", out)
			}
		default:
			if rule != (toolAfterRule{}) {
				t.Fatalf("call %d: level %d must not re-fire, got rule=%+v", i, g.level, rule)
			}
		}
	}
	if g.steers != 3 {
		t.Errorf("steers=%d, want one per level (3)", g.steers)
	}
}

// In the escalate arm a write re-arms the levels; the default arm's latch is
// deliberately different (see TestExploreGuardDefaultSteersOnce...).
func TestExploreGuardEscalateRearmsAfterWrite(t *testing.T) {
	h := &ChatHarness{}
	g := newExploreGuard()
	var events []string

	for i := 0; i < 6; i++ {
		exploreCall(t, h, g, codeExecGuardEscalate, 6, "execute_code", &events)
	}
	if g.level != 1 {
		t.Fatalf("level=%d, want 1", g.level)
	}
	var out string
	h.afterToolCall(schemas.ToolCall{Name: "write_file", Arguments: map[string]any{"path": "a.md"}},
		&out, g, codeExecGuardEscalate, 6, nil, func(ChatEvent) error { return nil })
	if g.level != 0 || g.streak != 0 {
		t.Fatalf("write must re-arm the escalate levels, got level=%d streak=%d", g.level, g.streak)
	}
	for i := 0; i < 6; i++ {
		exploreCall(t, h, g, codeExecGuardEscalate, 6, "search_archive", &events)
	}
	if g.level != 1 || g.steers != 2 {
		t.Errorf("after a write the next 6-call streak must steer again, got level=%d steers=%d", g.level, g.steers)
	}
}

// The level-2 batch stop must not swallow a write the model already queued.
func TestBatchHasWrite(t *testing.T) {
	h := &ChatHarness{}
	if !h.batchHasWrite([]schemas.ToolCall{
		{Name: "execute_code"},
		{Name: "write_file", Arguments: map[string]any{"path": "report.md"}},
	}) {
		t.Error("a queued write_file must count as a write")
	}
	if h.batchHasWrite([]schemas.ToolCall{{Name: "execute_code"}, {Name: "read_file", Arguments: map[string]any{"path": "x"}}}) {
		t.Error("exploration calls alone are not a queued write")
	}
}

// Every tool call needs exactly one tool result; skipped calls get a synthetic
// one instead of silence.
func TestSkipRemainingCallsClosesTheTranscript(t *testing.T) {
	var messages []ChatMessage
	var events []string
	skipRemainingCalls(
		[]schemas.ToolCall{{Name: "read_file"}, {Name: "execute_code"}},
		func(ev ChatEvent) error { events = append(events, ev.Type); return nil },
		&messages)
	if len(messages) != 2 {
		t.Fatalf("messages=%d, want one per skipped call", len(messages))
	}
	for _, m := range messages {
		if m.Role != "tool" || !strings.Contains(m.Content, "not executed") {
			t.Errorf("skipped call must be closed with a tool message, got %+v", m)
		}
	}
	if len(events) != 2 {
		t.Errorf("events=%v, want a tool_result per skipped call", events)
	}
}

func TestExplorationCeilingIsPartialRun(t *testing.T) {
	if !IsPartialRun(ErrExplorationExhausted) {
		t.Error("an exploration ceiling must count as a deliberate partial")
	}
	if !IsPartialRun(ErrMaxTurnsExceeded) {
		t.Error("a max-turns ceiling must still count as a deliberate partial")
	}
	if IsMaxTurnsPartial(ErrExplorationExhausted) {
		t.Error("the two ceilings have different causes; IsMaxTurnsPartial must not conflate them")
	}
	got := markExplorationTruncated("verified three files", 24)
	if !strings.HasPrefix(got, ExplorationTruncatedPrefix) || !strings.Contains(got, "24 calls") {
		t.Errorf("marker must carry the cause and the count, got %q", got)
	}
	if markExplorationTruncated("", 24) != "" {
		t.Error("an empty wreck must stay empty")
	}
}

// ── Run wiring: the escalate arm really ends the run ─────────────────────

// execLoopProvider always asks for one execute_code call, so the streak only
// ends when the guard ends it.
type execLoopProvider struct{ calls int }

func (p *execLoopProvider) Complete(ctx context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, schemas.CompleteRequest{}, func(string) error { return nil })
}

func (p *execLoopProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	onChunk("working")
	return &schemas.ProviderResponse{
		Text:      "working",
		ToolCalls: []schemas.ToolCall{{Name: "execute_code", Arguments: map[string]any{"code": "print(1)"}}},
	}, nil
}

func (p *execLoopProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
func (p *execLoopProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}
func (p *execLoopProvider) Name() string { return "exec-loop-mock" }

// Regression test for the Run wiring of the escalate arm: the helper tests
// prove the levels fire, this proves Run honours endRun and returns the
// exploration sentinel instead of running to the turn budget. Limit=2 is set
// through the env so level 3 lands on the 8th call, not the 24th.
func TestRunEscalateEndsAtExplorationCeiling(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		if _, err := exec.LookPath("python"); err != nil {
			t.Skip("no python runtime on PATH; execute_code cannot run here")
		}
	}
	t.Setenv(CodeExecGuardEnv, "escalate")
	t.Setenv(CodeExecStreakEnv, "2")

	provider := &execLoopProvider{}
	h := &ChatHarness{
		Provider:   provider,
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   20,
	}
	var events []ChatEvent
	// The mock writes one text chunk per turn, so finalText is non-empty and
	// the truncation marker has something to label.
	_, err := h.Run(context.Background(), "task-explore-run",
		[]ChatMessage{{Role: "user", Content: "keep exploring"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if !errors.Is(err, ErrExplorationExhausted) {
		t.Fatalf("want ErrExplorationExhausted, got %v", err)
	}
	if !IsPartialRun(err) {
		t.Error("the ceiling must be treated as a deliberate partial")
	}
	// limit=2 ⇒ levels at 2, 4, 8. The eighth execute_code call ends the run;
	// no ninth provider call may happen.
	if provider.calls != 8 {
		t.Errorf("provider calls=%d, want 8 (the run must end at 4*limit)", provider.calls)
	}
	var levels []any
	for _, ev := range events {
		if ev.Type == "exploration_escalated" {
			levels = append(levels, ev.Meta["level"])
		}
	}
	if len(levels) != 3 {
		t.Fatalf("exploration_escalated events=%v, want levels 1,2,3", levels)
	}
}
