package core

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestIsReadOnlyToolName pins the classifier behind the zero-effect completion
// gate (ADDED 2026-10-21, eval v14).
//
// Why this test exists at all: the gate asks "could anything this step did have
// changed state?" and the only input is tool names. Getting the classification
// wrong in the permissive direction silently disables the gate (it never fires
// on the tasks it was built for); wrong in the strict direction it fires on
// real work and the model gets told to redo what it already did. Both failure
// modes were observed while building this — see the note on download_ below.
func TestIsReadOnlyToolName(t *testing.T) {
	readOnly := []string{
		// The exact trace of AppWorld task d4e9306_1 in 4/5 runs.
		"supervisor__show_profile",
		"supervisor__show_account_passwords",
		"supervisor__show_active_task",
		"spotify__show_album", "spotify__search_songs", "spotify__get_song",
		"venmo__search_payments", "venmo__list_transactions",
		"gmail__fetch_emails", "amazon__count_orders",
		// Core tools classified by the same rule.
		"read_file",
	}
	for _, name := range readOnly {
		if !IsReadOnlyToolName(name) {
			t.Errorf("%s must be read-only", name)
		}
	}

	notReadOnly := []string{
		// Real AppWorld writes that IsWriteMCPTool ALSO misses (the reason this
		// gate uses a read-only whitelist instead of the write-verb blacklist).
		"spotify__download_song",
		"venmo__remind_payment_request",
		"venmo__deny_payment_request",
		"spotify__unfollow_artist",
		"spotify__unlike_song",
		"amazon__place_order",
		"file_system__move_file",
		// Explicitly must not be treated as "the work is done".
		"supervisor__complete_task",
		// Core tools with effects.
		"write_file", "execute_code",
		// Unknown verb → assume it might mutate.
		"frobnicate_widget",
	}
	for _, name := range notReadOnly {
		if IsReadOnlyToolName(name) {
			t.Errorf("%s must NOT be read-only (gate would be suppressed or misfire)", name)
		}
	}

	if IsReadOnlyToolName("") {
		t.Error("empty name must not be read-only")
	}
}

// TestZeroEffectGateTraceVerdict replays the trace shapes actually observed in
// the AppWorld runs through the same reduction the gate performs.
func TestZeroEffectGateTraceVerdict(t *testing.T) {
	// hasEffect mirrors the loop in spawner_dospawn.go: any non-read-only call
	// counts as having done something.
	hasEffect := func(tools []string) bool {
		for _, name := range tools {
			if !IsReadOnlyToolName(name) {
				return true
			}
		}
		return false
	}

	cases := []struct {
		name  string
		tools []string
		want  bool
	}{
		{"d4e9306_1 (observed 4/5 runs, then claimed ok at 0.90)", []string{
			"supervisor__show_profile",
			"supervisor__show_account_passwords",
		}, false},
		{"d4e9306_1 after a real follow", []string{
			"supervisor__show_profile",
			"spotify__follow_artist",
			"supervisor__complete_task",
		}, true},
		{"3ab5b8b_1 (download all liked songs)", []string{
			"supervisor__show_profile",
			"spotify__show_playlist",
			"spotify__download_song",
		}, true},
		{"23cf851_3 (pure count question)", []string{
			"venmo__show_transactions",
			"venmo__show_profile",
		}, false},
		{"empty trace (handled by noToolCallsYet, not this gate)", nil, false},
	}
	for _, c := range cases {
		if got := hasEffect(c.tools); got != c.want {
			t.Errorf("%s: hasEffect=%v, want %v", c.name, got, c.want)
		}
	}
}

// ─── A2 e2e: the gate must catch a parse-collapse after read-only work ────
//
// Reuses the scripted-provider harness from eval_spin_breaker_e2e_test.go
// (newSpinLoopEngine + registerSpinProvider, same package).

// zeroEffectCollapseProvider replays the v20 collapse shape (task 6c2c621_1):
//
//	turn 0 — one read-only domain call (lands in trace; fails harmlessly
//	          because no MCP is registered — the gate only reads tool names);
//	turn 1 — plain prose with no JSON (ParseOutput → partial / 0.3 with the
//	          "not parseable" assumption ⇒ parseFailed; no pseudo-tags, so the
//	          A1 guard does not intercept first);
//	turn 2+ — valid ok JSON, completing the step.
//
// It snapshots the last flattened user context it was handed so the test can
// assert what actually reached the model.
type zeroEffectCollapseProvider struct {
	calls    atomic.Int64
	lastUser atomic.Value // string
}

const zeCollapseGarbage = "This is plain prose. It carries no tool call and no valid status/confidence/result JSON."

func (p *zeroEffectCollapseProvider) Name() string { return "mock-ze-collapse" }

func (p *zeroEffectCollapseProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}

func (p *zeroEffectCollapseProvider) respond() *schemas.ProviderResponse {
	switch p.calls.Load() {
	case 1:
		return &schemas.ProviderResponse{ToolCalls: []schemas.ToolCall{
			{Name: "venmo__show_transactions", Arguments: map[string]any{"limit": 1}},
		}}
	case 2:
		return &schemas.ProviderResponse{Text: zeCollapseGarbage}
	default:
		return &schemas.ProviderResponse{Text: `{"status":"ok","confidence":0.9,"result":{"summary":"done"}}`}
	}
}

func (p *zeroEffectCollapseProvider) Complete(_ context.Context, r schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	p.lastUser.Store(r.User)
	return p.respond(), nil
}

func (p *zeroEffectCollapseProvider) StreamComplete(_ context.Context, r schemas.CompleteRequest, _ func(string) error) (*schemas.ProviderResponse, error) {
	p.calls.Add(1)
	p.lastUser.Store(r.User)
	return p.respond(), nil
}

func (p *zeroEffectCollapseProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, nil
}

// TestZeroEffectGate_FiresOnParseFailureAfterReadOnlyWork is the A2 regression
// (eval FINDINGS.md §A2, 2026-10-22). Before the fix the gate required
// StatusOK, so the v20 collapse shape — read-only trace + an output that
// ParseOutput degraded to partial — produced zero corrective signal from any
// gate: ZERO-EFFECT NUDGE fired 0 times in 8 tasks, and 6c2c621_1 scored 1/8
// with the correct plan written inside unparseable text. After the fix the
// gate fires on parseFailed turns too and injects the parse-failure message
// variant, which must actually reach the model's next-turn context.
func TestZeroEffectGate_FiresOnParseFailureAfterReadOnlyWork(t *testing.T) {
	engine := newSpinLoopEngine(t)

	// The gate is opt-in (the eval config sets zero_effect_nudge=true).
	engine.Mu.Lock()
	engine.registry.System.FeatureFlags.ZeroEffectNudge = true
	engine.Mu.Unlock()

	prov := &zeroEffectCollapseProvider{}
	cfg := &config.ProviderConfig{
		Provider: "mock-ze-collapse",
		BaseURL:  "mock://ze-collapse",
		Model:    "collapse-1",
	}
	registerSpinProvider(t, cfg, prov)

	engine.Mu.Lock()
	engine.registry.Providers["collapse"] = cfg
	engine.registry.DefaultProvider = "collapse"
	if r := engine.registry.Roles["worker"]; r != nil {
		r.Provider = "collapse"
	}
	engine.Mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := engine.spawner.Spawn(ctx, &SpawnRequest{
		TaskID: "ze_task_1",
		StepID: "ze_step_1",
		RoleID: "worker",
		Task:   "list my recent transactions and cancel the recurring one",
	})
	if err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}
	if res == nil {
		t.Fatal("Spawn returned nil result")
	}

	calls := prov.calls.Load()
	lastUser, _ := prov.lastUser.Load().(string)
	t.Logf("[ze-branch] provider_calls=%d status=%v conf=%.2f", calls, res.Output.Status, res.Output.Confidence)

	// The recovery turn (valid ok JSON) only happens if the loop CONTINUED
	// past the garbage turn instead of silently accepting the degraded partial.
	if calls < 3 {
		t.Errorf("A2 REGRESSION: loop ended after %d provider calls — the parse-degraded turn was "+
			"accepted silently (status=%v conf=%.2f) instead of being nudged", calls, res.Output.Status, res.Output.Confidence)
	}
	// The parse-failure message variant must have reached the model's context.
	if !strings.Contains(lastUser, SpawnerZeroEffectParseFailMsg[:60]) {
		t.Errorf("A2 REGRESSION: parse-failure zero-effect message never reached the model context; "+
			"last user context tail: %.300s", lastUser)
	}
	if res.Output.Status != schemas.StatusOK {
		t.Errorf("A2 REGRESSION: expected recovered ok status, got %v (conf %.2f)", res.Output.Status, res.Output.Confidence)
	}
}
