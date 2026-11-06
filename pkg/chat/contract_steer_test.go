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

// contractProvider scripts a run and records, for every provider call, how many
// times the completion steer is present in the prompt the model receives.
//
// Counting in the *request* rather than in the emitted event is deliberate: the
// event says we sent it, the request proves the model could read it.
type contractProvider struct {
	turn      int
	steerSeen []int
	askSeen   []int
	// requests keeps the raw user prompt per call, for tests that assert on the
	// exact sentence the model could read (artifact_defects_test.go).
	requests []string
	script   []func() *schemas.ProviderResponse
}

func (p *contractProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *contractProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.steerSeen = append(p.steerSeen, strings.Count(req.User, core.MarkerAllArtifactsPresent))
	p.askSeen = append(p.askSeen, strings.Count(req.User, core.MarkerAllArtifactsAsk))
	p.requests = append(p.requests, req.User)
	idx := p.turn
	p.turn++
	if idx < len(p.script) {
		return p.script[idx](), nil
	}
	return &schemas.ProviderResponse{Text: "done"}, nil
}

func (p *contractProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (p *contractProvider) CountTokens(ctx context.Context, text string) (int, error) { return 0, nil }
func (p *contractProvider) Name() string                                              { return "contract-mock" }

func writeArtifact(path string) func() *schemas.ProviderResponse {
	return func() *schemas.ProviderResponse {
		return &schemas.ProviderResponse{
			Text: "writing " + path,
			ToolCalls: []schemas.ToolCall{
				{Name: "write_file", Arguments: map[string]any{"path": path, "content": "value"}},
			},
		}
	}
}

// stopWithText ends the scripted run: zero tool calls is what makes Run return
// instead of looping to MaxTurns, so this is the only exit the test can observe.
func stopWithText() func() *schemas.ProviderResponse {
	return func() *schemas.ProviderResponse {
		return &schemas.ProviderResponse{Text: "All done."}
	}
}

const contractPrompt = "Build the reconciliation outputs.\n\n" +
	"## Enforced workspace change contract\n\n" +
	"The controller compares the final state of the workspace after every round.\n\n" +
	"- Create: `a.csv`, `b.json`\n" +
	"- Modify: none\n" +
	"- Delete: none\n"

func countEvents(events []ChatEvent, typ string) int {
	n := 0
	for _, ev := range events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// The load-bearing scenario: the steer appears exactly once, and only after the
// last declared artifact lands.
func TestContractSteer_FiresOnceWhenAllArtifactsExist(t *testing.T) {
	p := &contractProvider{script: []func() *schemas.ProviderResponse{
		writeArtifact("a.csv"),  // one of two: no steer yet
		writeArtifact("b.json"), // now complete: steer rides on this result
		writeArtifact("c.txt"),  // unrelated write: must NOT steer again
		stopWithText(),
	}}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: t.TempDir(),
		MaxTurns:    6,
	}

	var events []ChatEvent
	if _, err := h.Run(context.Background(), "task-contract",
		[]ChatMessage{{Role: "user", Content: contractPrompt}},
		func(ev ChatEvent) error { events = append(events, ev); return nil }); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// call 0 = first turn (nothing written yet)
	// call 1 = sees turn 1's result (a.csv only) -> still nothing
	// call 2 = sees turn 2's result -> steer present
	// call 3 = sees turn 3's result -> still exactly one copy
	want := []int{0, 0, 1, 1}
	if len(p.steerSeen) != len(want) {
		t.Fatalf("provider called %d times (%v), want %d", len(p.steerSeen), p.steerSeen, len(want))
	}
	for i := range want {
		if p.steerSeen[i] != want[i] {
			t.Fatalf("steer count per provider call = %v, want %v", p.steerSeen, want)
		}
	}

	if n := countEvents(events, "artifacts_complete"); n != 1 {
		t.Fatalf("artifacts_complete emitted %d times, want exactly 1", n)
	}
	// The event carries how many artifacts the prompt declared. That number is
	// the only way to tell "no contract in this prompt" apart from "contract
	// present, never satisfied" once the steer count reads 0 in a real batch,
	// so it is asserted here rather than left to the log line.
	for _, ev := range events {
		if ev.Type != "artifacts_complete" {
			continue
		}
		if got, _ := ev.Meta["declared"].(int); got != 2 {
			t.Fatalf("declared=%v, want 2", ev.Meta["declared"])
		}
	}
}

// Incomplete work must never be told it is complete — that is the failure the
// plan warns about (a premature stop with present < declared).
func TestContractSteer_SilentWhenArtifactsMissing(t *testing.T) {
	p := &contractProvider{script: []func() *schemas.ProviderResponse{
		writeArtifact("a.csv"), // b.json never appears
		stopWithText(),
	}}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: t.TempDir(),
		MaxTurns:    4,
	}

	var events []ChatEvent
	if _, err := h.Run(context.Background(), "task-partial",
		[]ChatMessage{{Role: "user", Content: contractPrompt}},
		func(ev ChatEvent) error { events = append(events, ev); return nil }); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	for i, n := range p.steerSeen {
		if n != 0 {
			t.Fatalf("call %d saw the steer with artifacts missing: %v", i, p.steerSeen)
		}
	}
	if n := countEvents(events, "artifacts_complete"); n != 0 {
		t.Fatalf("artifacts_complete emitted %d times, want 0", n)
	}
}

// No contract section means no contract: behaviour must be byte-identical to
// before this feature, which is what keeps every non-benchmark deployment safe.
//
// The prompt deliberately names both artifacts in backticks, and the script
// deliberately creates both. Without that pairing this test would pass for the
// wrong reason — the names would simply never be created, so *any* parser
// (including a whole-text backtick scan with no section check) would stay
// silent. With it, a parser that drops the section requirement fires the steer
// at turn 1 and this test goes red.
func TestContractSteer_SilentWithoutContractSection(t *testing.T) {
	p := &contractProvider{script: []func() *schemas.ProviderResponse{
		writeArtifact("a.csv"),
		writeArtifact("b.json"),
		stopWithText(),
	}}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: t.TempDir(),
		MaxTurns:    4,
	}

	var events []ChatEvent
	if _, err := h.Run(context.Background(), "task-plain",
		[]ChatMessage{{Role: "user", Content: "Just write `a.csv` and `b.json` please."}},
		func(ev ChatEvent) error { events = append(events, ev); return nil }); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	for i, n := range p.steerSeen {
		if n != 0 {
			t.Fatalf("call %d saw the steer without a contract: %v", i, p.steerSeen)
		}
	}
}

// Legacy layout: with no SessionRoot the artifacts land under outputs/<taskID>,
// so the declared names resolve against nothing meaningful. Falling back to the
// process working directory — which is what os.Stat("a.csv") does — is not a
// harmless default: it points at the deploy directory, where a colliding name
// would make the engine announce "all artifacts exist" over an empty workspace.
// That is the premature-stop failure this whole feature exists to prevent.
//
// The colliding files are planted on purpose. Without them the test passes for
// the wrong reason (the names happen to be absent from pkg/chat), and deleting
// the SessionRoot guard would not turn it red.
func TestContractSteer_SilentInLegacyLayout(t *testing.T) {
	cwd := t.TempDir()
	for name, body := range map[string]string{"a.csv": "planted\n", "b.json": "{}\n"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0644); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
	}
	t.Chdir(cwd)

	p := &contractProvider{script: []func() *schemas.ProviderResponse{
		writeArtifact("a.csv"),
		writeArtifact("b.json"),
		stopWithText(),
	}}
	h := &ChatHarness{
		Provider:   p,
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   4,
		// SessionRoot deliberately empty.
	}

	var events []ChatEvent
	if _, err := h.Run(context.Background(), "task-legacy",
		[]ChatMessage{{Role: "user", Content: contractPrompt}},
		func(ev ChatEvent) error { events = append(events, ev); return nil }); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	for i, n := range p.steerSeen {
		if n != 0 {
			t.Fatalf("call %d saw the steer in the legacy layout: %v", i, p.steerSeen)
		}
	}
}

// ── Mode arms (VORTEX_COMPLETION_CONTRACT) ────────────────────────────
//
// One image, three behaviours. The point of separating them at runtime is the
// 011 result: v70's authorize wording was followed by 5/14 runs that never read
// a single source file, so "which sentence did this run see" must not be
// inferred from which image it ran.

// off must suppress the text but keep the observation. Suppressing the event
// too would make arm "off" indistinguishable from a contract that was never
// satisfiable — and that is precisely the arm we need to compare against.
func TestContractSteer_OffEmitsEventWithoutText(t *testing.T) {
	t.Setenv(core.CompletionContractEnv, "off")
	p := &contractProvider{script: []func() *schemas.ProviderResponse{
		writeArtifact("a.csv"),
		writeArtifact("b.json"),
		stopWithText(),
	}}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: t.TempDir(),
		MaxTurns:    6,
	}

	var events []ChatEvent
	if _, err := h.Run(context.Background(), "task-off",
		[]ChatMessage{{Role: "user", Content: contractPrompt}},
		func(ev ChatEvent) error { events = append(events, ev); return nil }); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	for i, n := range p.steerSeen {
		if n != 0 {
			t.Fatalf("call %d saw the steer in off mode: %v", i, p.steerSeen)
		}
	}
	if n := countEvents(events, "artifacts_complete"); n != 1 {
		t.Fatalf("artifacts_complete emitted %d times, want exactly 1 even in off mode", n)
	}
	for _, ev := range events {
		if ev.Type == "artifacts_complete" && ev.Meta["mode"] != "off" {
			t.Fatalf("event mode=%v, want off — the arm has to be readable from the trace", ev.Meta["mode"])
		}
	}
}

// ask must swap the sentence, not just lengthen it: the license to stop has to
// be gone. Asserting only "the ask text is present" would stay green if the
// authorize sentence were also appended, which is the regression that matters.
func TestContractSteer_AskReplacesTheLicense(t *testing.T) {
	t.Setenv(core.CompletionContractEnv, "ask")
	p := &contractProvider{script: []func() *schemas.ProviderResponse{
		writeArtifact("a.csv"),
		writeArtifact("b.json"),
		stopWithText(),
	}}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: t.TempDir(),
		MaxTurns:    6,
	}

	var events []ChatEvent
	if _, err := h.Run(context.Background(), "task-ask",
		[]ChatMessage{{Role: "user", Content: contractPrompt}},
		func(ev ChatEvent) error { events = append(events, ev); return nil }); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if total := sumInts(p.steerSeen); total != 0 {
		t.Fatalf("authorize wording still present in ask mode (%d occurrences): %v", total, p.steerSeen)
	}
	if total := sumInts(p.askSeen); total != 1 {
		t.Fatalf("ask wording appeared %d times, want exactly 1: %v", total, p.askSeen)
	}
	for _, ev := range events {
		if ev.Type == "artifacts_complete" && ev.Meta["mode"] != "ask" {
			t.Fatalf("event mode=%v, want ask", ev.Meta["mode"])
		}
	}
}

// An unrecognised value must not quietly run the default arm: a typo in a batch
// script would then look exactly like a valid run of the behavior we already
// know early-stops (→107).
func TestContractSteer_UnknownModeFallsBackButSaysSo(t *testing.T) {
	for _, raw := range []string{"of", "AUTHORISE", " ask please "} {
		mode, ok := core.ParseCompletionContractMode(raw)
		if mode != core.CompletionContractAuthorize {
			t.Fatalf("ParseCompletionContractMode(%q) = %v, want authorize", raw, mode)
		}
		if ok {
			t.Fatalf("ParseCompletionContractMode(%q) reported ok, want not ok", raw)
		}
	}
	for raw, want := range map[string]core.CompletionContractMode{
		"":          core.CompletionContractAuthorize,
		"authorize": core.CompletionContractAuthorize,
		" OFF ":     core.CompletionContractOff,
		"ask":       core.CompletionContractAsk,
		"Verify":    core.CompletionContractAsk,
	} {
		mode, ok := core.ParseCompletionContractMode(raw)
		if !ok || mode != want {
			t.Fatalf("ParseCompletionContractMode(%q) = %v,%v want %v,true", raw, mode, ok, want)
		}
	}
}

func sumInts(v []int) int {
	n := 0
	for _, x := range v {
		n += x
	}
	return n
}
