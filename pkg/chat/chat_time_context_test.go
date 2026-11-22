package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// --- Unit tests for helper functions ---

func TestWeekdayEn(t *testing.T) {
	cases := []struct {
		date string
		want string
	}{
		{"2026-11-20T00:00:00+08:00", "Friday"},
		{"2026-11-19T00:00:00+08:00", "Thursday"},
		{"2026-11-22T00:00:00+08:00", "Sunday"},
		{"2026-11-23T00:00:00+08:00", "Monday"},
	}
	for _, c := range cases {
		tm, err := time.Parse(time.RFC3339, c.date)
		if err != nil {
			t.Fatalf("parse %s: %v", c.date, err)
		}
		if got := weekdayEn(tm); got != c.want {
			t.Errorf("weekdayEn(%s) = %q, want %q", c.date, got, c.want)
		}
	}
}

func TestFirstCreatedAt(t *testing.T) {
	t.Run("non-empty history", func(t *testing.T) {
		ts := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		h := []ChatMessage{{Role: "user", Content: "hi", CreatedAt: ts}}
		if got := firstCreatedAt(h); !got.Equal(ts) {
			t.Errorf("firstCreatedAt = %v, want %v", got, ts)
		}
	})
	t.Run("empty history", func(t *testing.T) {
		if got := firstCreatedAt(nil); !got.IsZero() {
			t.Errorf("firstCreatedAt(nil) = %v, want zero", got)
		}
		if got := firstCreatedAt([]ChatMessage{}); !got.IsZero() {
			t.Errorf("firstCreatedAt([]) = %v, want zero", got)
		}
	})
}

func TestPrevUserGap(t *testing.T) {
	t1 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	wantGap := t2.Sub(t1)

	t.Run("two user messages", func(t *testing.T) {
		h := []ChatMessage{
			{Role: "user", Content: "first", CreatedAt: t1},
			{Role: "assistant", Content: "reply", CreatedAt: t1.Add(1 * time.Second)},
			{Role: "user", Content: "second", CreatedAt: t2},
		}
		gap, ok := prevUserGap(h)
		if !ok {
			t.Fatal("prevUserGap returned false, want true")
		}
		if gap != wantGap {
			t.Errorf("gap = %v, want %v", gap, wantGap)
		}
	})
	t.Run("only one user message", func(t *testing.T) {
		h := []ChatMessage{{Role: "user", Content: "only", CreatedAt: t1}}
		_, ok := prevUserGap(h)
		if ok {
			t.Fatal("prevUserGap returned true for single user message, want false")
		}
	})
	t.Run("zero CreatedAt", func(t *testing.T) {
		h := []ChatMessage{
			{Role: "user", Content: "a", CreatedAt: time.Time{}},
			{Role: "user", Content: "b", CreatedAt: t2},
		}
		_, ok := prevUserGap(h)
		if ok {
			t.Fatal("prevUserGap returned true with zero CreatedAt, want false")
		}
	})
	t.Run("no user messages", func(t *testing.T) {
		h := []ChatMessage{{Role: "assistant", Content: "hi", CreatedAt: t1}}
		_, ok := prevUserGap(h)
		if ok {
			t.Fatal("prevUserGap returned true with no user messages, want false")
		}
	})
}

// --- Unit tests for buildTimeContext ---

func TestBuildTimeContext_Content(t *testing.T) {
	now := time.Date(2026, 11, 20, 14, 32, 7, 0, time.FixedZone("CST", 8*3600))
	opened := time.Date(2026, 11, 19, 11, 50, 44, 0, time.FixedZone("CST", 8*3600))
	prevUser := time.Date(2026, 11, 20, 2, 29, 56, 0, time.FixedZone("CST", 8*3600))
	lastUser := time.Date(2026, 11, 20, 14, 32, 7, 0, time.FixedZone("CST", 8*3600))

	h := []ChatMessage{
		{Role: "user", Content: "start", CreatedAt: opened},
		{Role: "assistant", Content: "reply1", CreatedAt: opened.Add(5 * time.Minute)},
		{Role: "user", Content: "continue", CreatedAt: prevUser},
		{Role: "assistant", Content: "reply2", CreatedAt: prevUser.Add(2 * time.Minute)},
		{Role: "user", Content: "now?", CreatedAt: lastUser},
	}

	out := buildTimeContext(now, h)

	if !strings.Contains(out, "## Time context") {
		t.Errorf("missing header: %q", out)
	}
	if !strings.Contains(out, "Now: 2026-11-20T14:32:07+08:00 (Friday)") {
		t.Errorf("missing/incorrect Now line: %q", out)
	}
	if !strings.Contains(out, "Session opened: 2026-11-19T11:50:44+08:00") {
		t.Errorf("missing/incorrect Session opened line: %q", out)
	}
	if !strings.Contains(out, "Session elapsed:") {
		t.Errorf("missing Session elapsed line: %q", out)
	}
	if !strings.Contains(out, "Gap since previous user message:") {
		t.Errorf("missing Gap line: %q", out)
	}
}

func TestBuildTimeContext_GapSemantics(t *testing.T) {
	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	prevUser := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
	lastUser := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)

	t.Run("12h gap between user messages", func(t *testing.T) {
		h := []ChatMessage{
			{Role: "user", Content: "morning", CreatedAt: prevUser},
			{Role: "assistant", Content: "reply", CreatedAt: prevUser.Add(1 * time.Minute)},
			{Role: "user", Content: "evening", CreatedAt: lastUser},
		}
		out := buildTimeContext(now, h)
		if !strings.Contains(out, "Gap since previous user message: 12h0m0s") {
			t.Errorf("expected 12h gap, got: %q", out)
		}
	})

	t.Run("first turn no gap line", func(t *testing.T) {
		h := []ChatMessage{{Role: "user", Content: "hello", CreatedAt: lastUser}}
		out := buildTimeContext(now, h)
		if strings.Contains(out, "Gap since previous user message") {
			t.Errorf("first turn should not have Gap line: %q", out)
		}
	})
}

func TestBuildTimeContext_ZeroValueDefense(t *testing.T) {
	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)

	t.Run("all CreatedAt zero", func(t *testing.T) {
		h := []ChatMessage{
			{Role: "user", Content: "a", CreatedAt: time.Time{}},
			{Role: "assistant", Content: "b", CreatedAt: time.Time{}},
			{Role: "user", Content: "c", CreatedAt: time.Time{}},
		}
		out := buildTimeContext(now, h)
		if !strings.Contains(out, "## Time context") {
			t.Errorf("missing header: %q", out)
		}
		if !strings.Contains(out, "Now:") {
			t.Errorf("missing Now line: %q", out)
		}
		if strings.Contains(out, "Session opened") {
			t.Errorf("should not have Session opened with zero CreatedAt: %q", out)
		}
		if strings.Contains(out, "Session elapsed") {
			t.Errorf("should not have Session elapsed with zero CreatedAt: %q", out)
		}
		if strings.Contains(out, "Gap") {
			t.Errorf("should not have Gap with zero CreatedAt: %q", out)
		}
	})

	t.Run("mixed zero and valid", func(t *testing.T) {
		valid := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
		h := []ChatMessage{
			{Role: "user", Content: "a", CreatedAt: valid},
			{Role: "assistant", Content: "b", CreatedAt: time.Time{}},
			{Role: "user", Content: "c", CreatedAt: time.Time{}},
		}
		out := buildTimeContext(now, h)
		if !strings.Contains(out, "Session opened: 2026-11-19T10:00:00Z") {
			t.Errorf("should have Session opened for valid first message: %q", out)
		}
		if strings.Contains(out, "Gap") {
			t.Errorf("should not have Gap when last user CreatedAt is zero: %q", out)
		}
	})
}

func TestBuildTimeContext_KillSwitch(t *testing.T) {
	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	h := []ChatMessage{{Role: "user", Content: "hi", CreatedAt: now}}

	t.Run("0 disables", func(t *testing.T) {
		t.Setenv("VORTEX_CHAT_TIME_CONTEXT", "0")
		if out := buildTimeContext(now, h); out != "" {
			t.Errorf("VORTEX_CHAT_TIME_CONTEXT=0 should return empty, got %q", out)
		}
	})

	t.Run("off disables", func(t *testing.T) {
		t.Setenv("VORTEX_CHAT_TIME_CONTEXT", "off")
		if out := buildTimeContext(now, h); out != "" {
			t.Errorf("VORTEX_CHAT_TIME_CONTEXT=off should return empty, got %q", out)
		}
	})

	t.Run("unset enables", func(t *testing.T) {
		t.Setenv("VORTEX_CHAT_TIME_CONTEXT", "")
		if out := buildTimeContext(now, h); out == "" {
			t.Errorf("unset VORTEX_CHAT_TIME_CONTEXT should return content")
		}
	})

	t.Run("1 enables", func(t *testing.T) {
		t.Setenv("VORTEX_CHAT_TIME_CONTEXT", "1")
		if out := buildTimeContext(now, h); out == "" {
			t.Errorf("VORTEX_CHAT_TIME_CONTEXT=1 should return content")
		}
	})

	t.Run("on enables", func(t *testing.T) {
		t.Setenv("VORTEX_CHAT_TIME_CONTEXT", "on")
		if out := buildTimeContext(now, h); out == "" {
			t.Errorf("VORTEX_CHAT_TIME_CONTEXT=on should return content")
		}
	})
}

func TestBuildTimeContext_FirstTurn(t *testing.T) {
	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	lastUser := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)

	h := []ChatMessage{{Role: "user", Content: "hello", CreatedAt: lastUser}}
	out := buildTimeContext(now, h)

	if strings.Contains(out, "Gap since previous user message") {
		t.Errorf("first turn should not have Gap line: %q", out)
	}
	if !strings.Contains(out, "Session opened: 2026-11-20T14:00:00Z") {
		t.Errorf("should have Session opened matching the single message: %q", out)
	}
	if !strings.Contains(out, "Session elapsed: 0s") {
		t.Errorf("should have near-zero Session elapsed: %q", out)
	}
}

func TestBuildTimeContext_EmptyHistory(t *testing.T) {
	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)

	t.Run("nil history", func(t *testing.T) {
		out := buildTimeContext(now, nil)
		if !strings.Contains(out, "## Time context") {
			t.Errorf("missing header: %q", out)
		}
		if !strings.Contains(out, "Now:") {
			t.Errorf("missing Now line: %q", out)
		}
		if strings.Contains(out, "Session opened") {
			t.Errorf("should not have Session opened for nil history: %q", out)
		}
		if strings.Contains(out, "Gap") {
			t.Errorf("should not have Gap for nil history: %q", out)
		}
	})

	t.Run("empty slice", func(t *testing.T) {
		out := buildTimeContext(now, []ChatMessage{})
		if !strings.Contains(out, "Now:") {
			t.Errorf("missing Now line: %q", out)
		}
	})
}

// --- Integration tests ---

// timeCtxProvider records req.User per provider call for asserting time
// context block presence and stability.
type timeCtxProvider struct {
	requests []string
	script   []func() *schemas.ProviderResponse
	turn     int
}

func (p *timeCtxProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *timeCtxProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.requests = append(p.requests, req.User)
	idx := p.turn
	p.turn++
	if idx < len(p.script) {
		return p.script[idx](), nil
	}
	return &schemas.ProviderResponse{Text: "done"}, nil
}

func (p *timeCtxProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (p *timeCtxProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (p *timeCtxProvider) Name() string                                              { return "timectx-mock" }

func TestTimeContext_InjectionPosition(t *testing.T) {
	t.Setenv("VORTEX_CHAT_TIME_CONTEXT", "1")

	now := time.Now()
	p := &timeCtxProvider{script: []func() *schemas.ProviderResponse{
		func() *schemas.ProviderResponse { return &schemas.ProviderResponse{Text: "ok"} },
	}}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: t.TempDir(),
		MaxTurns:    1,
	}

	_, err := h.Run(context.Background(), "task-time",
		[]ChatMessage{{Role: "user", Content: "what time is it?", CreatedAt: now}},
		func(ev ChatEvent) error { return nil })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if len(p.requests) == 0 {
		t.Fatal("provider not called")
	}

	userPrompt := p.requests[0]
	if !strings.Contains(userPrompt, "## Time context") {
		t.Errorf("time context block not in provider request: %q", userPrompt)
	}
	if !strings.Contains(userPrompt, "Now:") {
		t.Errorf("Now line not in provider request: %q", userPrompt)
	}
}

func TestTimeContext_SessionNotPolluted(t *testing.T) {
	t.Setenv("VORTEX_CHAT_TIME_CONTEXT", "1")

	now := time.Now()
	history := []ChatMessage{{Role: "user", Content: "hello", CreatedAt: now}}

	p := &timeCtxProvider{script: []func() *schemas.ProviderResponse{
		func() *schemas.ProviderResponse { return &schemas.ProviderResponse{Text: "ok"} },
	}}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: t.TempDir(),
		MaxTurns:    1,
	}

	_, err := h.Run(context.Background(), "task-pollute", history,
		func(ev ChatEvent) error { return nil })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// The original history slice must not be modified — time context is only
	// appended to the in-memory messages copy inside Run.
	if strings.Contains(history[0].Content, "## Time context") {
		t.Errorf("original history was polluted with time context: %q", history[0].Content)
	}
}

func TestTimeContext_RunInternalStability(t *testing.T) {
	t.Setenv("VORTEX_CHAT_TIME_CONTEXT", "1")

	now := time.Now()
	p := &timeCtxProvider{script: []func() *schemas.ProviderResponse{
		func() *schemas.ProviderResponse {
			return &schemas.ProviderResponse{
				Text: "working",
				ToolCalls: []schemas.ToolCall{
					{Name: "read_file", Arguments: map[string]any{"path": "test.txt"}},
				},
			}
		},
		func() *schemas.ProviderResponse { return &schemas.ProviderResponse{Text: "done"} },
	}}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: t.TempDir(),
		MaxTurns:    3,
	}

	_, err := h.Run(context.Background(), "task-stable",
		[]ChatMessage{{Role: "user", Content: "check time", CreatedAt: now}},
		func(ev ChatEvent) error { return nil })
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if len(p.requests) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", len(p.requests))
	}

	tc1 := extractTimeContext(p.requests[0])
	tc2 := extractTimeContext(p.requests[1])
	if tc1 == "" {
		t.Fatal("turn 1 has no time context block")
	}
	if tc1 != tc2 {
		t.Errorf("time context not stable across turns:\n  turn1: %q\n  turn2: %q", tc1, tc2)
	}
}

// extractTimeContext pulls the "## Time context" block from a flattened
// prompt string for byte-level comparison. The block is the header plus
// lines starting with "Now:", "Session", or "Gap since".
func extractTimeContext(prompt string) string {
	start := strings.Index(prompt, "## Time context")
	if start == -1 {
		return ""
	}
	rest := prompt[start:]
	lines := strings.Split(rest, "\n")
	var block []string
	for i, line := range lines {
		if i == 0 {
			block = append(block, line)
			continue
		}
		if strings.HasPrefix(line, "Now:") ||
			strings.HasPrefix(line, "Session opened:") ||
			strings.HasPrefix(line, "Session elapsed:") ||
			strings.HasPrefix(line, "Gap since previous user message:") {
			block = append(block, line)
		} else {
			break
		}
	}
	return strings.Join(block, "\n")
}

// --- P1: Sparse history time marks tests ---

func TestFlattenChatMessages_NoMarksByDefault(t *testing.T) {
	t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(3 * time.Hour)
	msgs := []ChatMessage{
		{Role: "user", Content: "hello", CreatedAt: t1},
		{Role: "assistant", Content: "hi", CreatedAt: t1.Add(1 * time.Second)},
		{Role: "user", Content: "later", CreatedAt: t2},
	}
	out := flattenChatMessages(msgs)
	if strings.Contains(out, "later ---") {
		t.Errorf("should not have time marks when VORTEX_CHAT_TIME_MARKS unset: %q", out)
	}
}

func TestFlattenChatMessages_MarksWhenEnabled(t *testing.T) {
	t.Setenv("VORTEX_CHAT_TIME_MARKS", "10m")
	t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(3 * time.Hour)
	msgs := []ChatMessage{
		{Role: "user", Content: "hello", CreatedAt: t1},
		{Role: "assistant", Content: "hi", CreatedAt: t1.Add(1 * time.Second)},
		{Role: "user", Content: "later", CreatedAt: t2},
	}
	out := flattenChatMessages(msgs)
	if !strings.Contains(out, "--- 3h00m later ---") {
		t.Errorf("expected 3h00m mark between messages, got: %q", out)
	}
}

func TestFlattenChatMessages_NoMarkForSmallGap(t *testing.T) {
	t.Setenv("VORTEX_CHAT_TIME_MARKS", "10m")
	t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	msgs := []ChatMessage{
		{Role: "user", Content: "hello", CreatedAt: t1},
		{Role: "assistant", Content: "hi", CreatedAt: t1.Add(5 * time.Minute)},
	}
	out := flattenChatMessages(msgs)
	if strings.Contains(out, "later ---") {
		t.Errorf("5m gap should not produce a mark with 10m threshold: %q", out)
	}
}

func TestFlattenChatMessages_MarkAtThresholdBoundary(t *testing.T) {
	t.Setenv("VORTEX_CHAT_TIME_MARKS", "10m")
	t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(10 * time.Minute)
	msgs := []ChatMessage{
		{Role: "user", Content: "hello", CreatedAt: t1},
		{Role: "assistant", Content: "hi", CreatedAt: t2},
	}
	out := flattenChatMessages(msgs)
	if !strings.Contains(out, "--- 10m later ---") {
		t.Errorf("10m gap at 10m threshold should produce a mark: %q", out)
	}
}

func TestFlattenChatMessages_MultipleMarks(t *testing.T) {
	t.Setenv("VORTEX_CHAT_TIME_MARKS", "10m")
	t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(2 * time.Hour)
	t3 := t2.Add(30 * time.Minute)
	msgs := []ChatMessage{
		{Role: "user", Content: "first", CreatedAt: t1},
		{Role: "assistant", Content: "reply1", CreatedAt: t1.Add(1 * time.Minute)},
		{Role: "user", Content: "second", CreatedAt: t2},
		{Role: "assistant", Content: "reply2", CreatedAt: t2.Add(1 * time.Minute)},
		{Role: "user", Content: "third", CreatedAt: t3},
	}
	out := flattenChatMessages(msgs)
	if !strings.Contains(out, "--- 1h59m later ---") {
		t.Errorf("expected 1h59m mark, got: %q", out)
	}
	if !strings.Contains(out, "--- 29m later ---") {
		t.Errorf("expected 29m mark, got: %q", out)
	}
}

func TestFlattenChatMessages_ZeroCreatedAtSkipsMark(t *testing.T) {
	t.Setenv("VORTEX_CHAT_TIME_MARKS", "10m")
	t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	msgs := []ChatMessage{
		{Role: "user", Content: "hello", CreatedAt: t1},
		{Role: "assistant", Content: "hi", CreatedAt: time.Time{}},
	}
	out := flattenChatMessages(msgs)
	if strings.Contains(out, "later ---") {
		t.Errorf("zero CreatedAt should not produce a mark: %q", out)
	}
}

func TestFlattenChatMessages_InvalidThresholdDisabled(t *testing.T) {
	t.Setenv("VORTEX_CHAT_TIME_MARKS", "garbage")
	t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(3 * time.Hour)
	msgs := []ChatMessage{
		{Role: "user", Content: "hello", CreatedAt: t1},
		{Role: "assistant", Content: "hi", CreatedAt: t2},
	}
	out := flattenChatMessages(msgs)
	if strings.Contains(out, "later ---") {
		t.Errorf("invalid threshold should disable marks: %q", out)
	}
}

func TestTimeMarksThreshold(t *testing.T) {
	t.Run("unset returns 0", func(t *testing.T) {
		t.Setenv("VORTEX_CHAT_TIME_MARKS", "")
		if d := timeMarksThreshold(); d != 0 {
			t.Errorf("unset should return 0, got %v", d)
		}
	})
	t.Run("10m parses", func(t *testing.T) {
		t.Setenv("VORTEX_CHAT_TIME_MARKS", "10m")
		if d := timeMarksThreshold(); d != 10*time.Minute {
			t.Errorf("10m should return 10m, got %v", d)
		}
	})
	t.Run("negative returns 0", func(t *testing.T) {
		t.Setenv("VORTEX_CHAT_TIME_MARKS", "-5m")
		if d := timeMarksThreshold(); d != 0 {
			t.Errorf("negative should return 0, got %v", d)
		}
	})
	t.Run("invalid returns 0", func(t *testing.T) {
		t.Setenv("VORTEX_CHAT_TIME_MARKS", "abc")
		if d := timeMarksThreshold(); d != 0 {
			t.Errorf("invalid should return 0, got %v", d)
		}
	})
}

func TestFormatGap(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m"},
		{10 * time.Minute, "10m"},
		{59 * time.Minute, "59m"},
		{60 * time.Minute, "1h00m"},
		{125 * time.Minute, "2h05m"},
		{3 * time.Hour, "3h00m"},
	}
	for _, c := range cases {
		if got := formatGap(c.d); got != c.want {
			t.Errorf("formatGap(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
