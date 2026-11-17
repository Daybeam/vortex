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

// The completion contract has two halves. Parsed artifacts answer "is the file
// there"; parsed fields answer "does it carry what the task said it must". A
// file can be there and still be the wrong shape, and on harness-bench 011 that
// was the difference between 9 failing runs and 9 passing ones: v71 wrote
// `candidates: [{type: "recommended", …}]` instead of a top-level
// `recommended`, with every window arithmetically correct.
//
// The property these tests pin is *which sentence the model receives*. "All
// declared artifacts exist" and "your plan is missing recommended" in the same
// breath is a false green: the model reads the first clause and stops. So the
// done steer must be suppressed while a declared field is absent, and the
// defect report must not consume the one-shot done latch — otherwise a run that
// fixes its plan after the report is never told it is finished.

const fieldsPrompt = "Build the scheduling handoff.\n\n" +
	"Create `$WORKSPACE/b.json` containing:\n\n" +
	"- `recommended` and `fallbacks`;\n" +
	"- for each candidate: `start` and `end`;\n\n" +
	"## Enforced workspace change contract\n\n" +
	"- Create: `b.json`\n"

// wrongPlan is the v71 shape: the information is all there, under a key the task
// never asked for.
const wrongPlan = `{"candidates":[{"type":"recommended","start":"2026-05-12T11:30:00-04:00"}]}`

// rightPlan satisfies the two declared keys. The values are deliberately empty:
// this test is about presence, and a richer fixture would make it look like the
// engine is judging whether the window is correct (it must not — that is the
// scorer's job).
const rightPlan = `{"recommended":{},"fallbacks":[]}`

func writeFileWith(path, content string) func() *schemas.ProviderResponse {
	return func() *schemas.ProviderResponse {
		return &schemas.ProviderResponse{
			Text: "writing " + path,
			ToolCalls: []schemas.ToolCall{
				{Name: "write_file", Arguments: map[string]any{"path": path, "content": content}},
			},
		}
	}
}

func runFieldsPrompt(t *testing.T, script []func() *schemas.ProviderResponse) (*contractProvider, []ChatEvent) {
	t.Helper()
	p := &contractProvider{script: script}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: t.TempDir(),
		MaxTurns:    8,
	}

	var events []ChatEvent
	if _, err := h.Run(context.Background(), "task-fields",
		[]ChatMessage{{Role: "user", Content: fieldsPrompt}},
		func(ev ChatEvent) error { events = append(events, ev); return nil }); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return p, events
}

func TestContractSteer_DefectSuppressesTheDoneSteer(t *testing.T) {
	p, events := runFieldsPrompt(t, []func() *schemas.ProviderResponse{
		writeFileWith("b.json", wrongPlan), // present, wrong shape
		stopWithText(),
	})

	if n := countEvents(events, "artifacts_complete"); n != 0 {
		t.Errorf("artifacts_complete fired %d times, want 0: the file exists but does not satisfy the contract", n)
	}
	if n := countEvents(events, "artifacts_incomplete"); n != 1 {
		t.Fatalf("artifacts_incomplete fired %d times, want exactly 1", n)
	}

	// Read the request, not the event: the event says we produced the sentence,
	// the request proves the model could see it.
	idx := indexOfCallContaining(p, "do not satisfy the contract")
	if idx < 0 {
		t.Fatalf("no provider call saw the defect report; requests were:\n%s", dumpSeen(p))
	}
	if strings.Contains(p.requests[idx], core.MarkerAllArtifactsPresent) {
		t.Error("the done steer and the defect report must never be in the same request")
	}
	if !strings.Contains(p.requests[idx], `"recommended"`) {
		t.Errorf("the report must name the missing key, got:\n%s", p.requests[idx])
	}
}

func TestContractSteer_DoneFiresAfterTheDefectIsFixed(t *testing.T) {
	p, events := runFieldsPrompt(t, []func() *schemas.ProviderResponse{
		writeFileWith("b.json", wrongPlan), // report
		writeFileWith("b.json", rightPlan), // fixed: now the latch may fire
		stopWithText(),
	})

	if n := countEvents(events, "artifacts_incomplete"); n != 1 {
		t.Errorf("artifacts_incomplete = %d, want 1", n)
	}
	if n := countEvents(events, "artifacts_complete"); n != 1 {
		t.Fatalf("artifacts_complete = %d, want 1: the defect report must not consume the done latch", n)
	}

	// The done sentence must appear strictly after the defect sentence — a run
	// that is told "done" before it is fixed is the false green this check is for.
	fixIdx := indexOfCallContaining(p, "do not satisfy the contract")
	doneIdx := indexOfCallContaining(p, core.MarkerAllArtifactsPresent)
	if fixIdx < 0 || doneIdx < 0 {
		t.Fatalf("defect=%d done=%d, both steers must reach a request\n%s", fixIdx, doneIdx, dumpSeen(p))
	}
	if doneIdx <= fixIdx {
		t.Errorf("done steer seen on call %d, defect report on call %d: done must come after", doneIdx, fixIdx)
	}
}

// A prompt that states no field block keeps the old behaviour exactly: files
// present is still enough. Without this case, a parser that invented fields for
// every task would look correct on the two tests above.
func TestContractSteer_NoFieldBlockKeepsExistenceOnly(t *testing.T) {
	p := &contractProvider{script: []func() *schemas.ProviderResponse{
		writeArtifact("a.csv"),            // contractPrompt declares a.csv too
		writeFileWith("b.json", wrongPlan), // shape is irrelevant without a field block
		stopWithText(),
	}}
	h := &ChatHarness{
		Provider:    p,
		Model:       "mock",
		OutputBase:  t.TempDir(),
		SessionRoot: t.TempDir(),
		MaxTurns:    8,
	}
	var events []ChatEvent
	// contractPrompt declares b.json but no field block.
	if _, err := h.Run(context.Background(), "task-fields-legacy",
		[]ChatMessage{{Role: "user", Content: contractPrompt}},
		func(ev ChatEvent) error { events = append(events, ev); return nil }); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if n := countEvents(events, "artifacts_complete"); n != 1 {
		t.Fatalf("artifacts_complete = %d, want 1 (no field contract, existence is enough)", n)
	}
	if n := countEvents(events, "artifacts_incomplete"); n != 0 {
		t.Errorf("artifacts_incomplete = %d, want 0", n)
	}
}

// indexOfCallContaining returns the index of the first provider request that
// contains needle, or -1.
func indexOfCallContaining(p *contractProvider, needle string) int {
	for i, req := range p.requests {
		if strings.Contains(req, needle) {
			return i
		}
	}
	return -1
}

func dumpSeen(p *contractProvider) string {
	var b strings.Builder
	for i, req := range p.requests {
		b.WriteString("--- call ")
		b.WriteString(string(rune('0' + i)))
		b.WriteString(" ---\n")
		b.WriteString(req)
		b.WriteString("\n")
	}
	return b.String()
}

// The delegation gate has the same two halves as the done steer: it exists to
// avoid paying for work the workspace already holds, so "the files are there"
// must not be the test. Otherwise a run whose plan is shaped wrongly gets its
// delegation skipped *and* no repair, which is strictly worse than delegating.
func TestDelegationGate_MalformedArtifactIsNotDone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.csv"), []byte("id,total\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.json"), []byte(wrongPlan), 0644); err != nil {
		t.Fatal(err)
	}

	spy := &delegateSpy{}
	h := &ChatHarness{SessionRoot: root, Delegate: spy.delegate}
	h.declaredArtifacts = []string{"a.csv", "b.json"}
	h.declaredFields = map[string][]string{"b.json": {"recommended", "fallbacks"}}

	out, err := h.execTool(context.Background(), "delegate_to_orchestrator",
		map[string]any{"prompt": "repair the plan"}, "chat_gate")
	if err != nil {
		t.Fatalf("execTool returned an error, want a receipt: %v", err)
	}
	if strings.HasPrefix(out, core.MarkerDelegationSkipped) {
		t.Fatalf("gate fired over an artifact that is present but malformed; the run would stop with a plan the scorer rejects:\n%s", out)
	}
	if spy.calls() != 1 {
		t.Fatalf("delegate calls = %d, want 1", spy.calls())
	}
}

// A run can be clean and then get worse. Until →226 the one-shot done latch also
// switched off defect reporting — `if contractSteers == 0 && …` wrapped both
// branches — so the sequence "pass the contract, then break the artifact" ended
// with the model having been told it was finished and never told otherwise. The
// archive agrees that this was structural rather than rare: across 386
// events.sse files there are 0 runs with `artifacts_complete` followed by
// `artifacts_incomplete`, and all 15 mixed runs are the opposite order.
//
// Both branches still need their own budget: the done sentence stays once per
// run (contractSteers), and defects stay capped by maxContractDefectSteers.
func TestContractSteer_DefectAfterTheDoneSteerStillReachesTheModel(t *testing.T) {
	p, events := runFieldsPrompt(t, []func() *schemas.ProviderResponse{
		writeFileWith("b.json", rightPlan), // clean → done steer, latch consumed
		writeFileWith("b.json", wrongPlan), // broken again, after the latch closed
		stopWithText(),
	})

	if n := countEvents(events, "artifacts_complete"); n != 1 {
		t.Fatalf("artifacts_complete = %d, want exactly 1: the done sentence is still one-shot", n)
	}
	if n := countEvents(events, "artifacts_incomplete"); n != 1 {
		t.Fatalf("artifacts_incomplete = %d, want 1: the done latch must not silence later defects (→226)", n)
	}

	// Read the requests, not just the events: the event says the sentence was
	// produced, the request proves the model could see it — and the order is the
	// whole point here.
	doneIdx := indexOfCallContaining(p, core.MarkerAllArtifactsPresent)
	badIdx := indexOfCallContaining(p, "do not satisfy the contract")
	if doneIdx < 0 || badIdx < 0 {
		t.Fatalf("done=%d defect=%d, both steers must reach a request\n%s", doneIdx, badIdx, dumpSeen(p))
	}
	if badIdx <= doneIdx {
		t.Errorf("defect report seen on call %d, done sentence on call %d: the report must come after", badIdx, doneIdx)
	}
}
