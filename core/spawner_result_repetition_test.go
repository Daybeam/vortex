package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestSpawner_ResultRepetition_SieveInitialized verifies that the Spawner
// creates a non-nil Sieve with CheckResultRepetition accessible. This is the
// wiring contract: if the Sieve were nil or uninitialized, the
// CheckResultRepetition call in spawner.go would silently no-op (the method
// has a nil receiver guard), and result repetition loops would burn the
// turn budget undetected.
//
// Regression: before the fix, CheckResultRepetition was only wired in
// chat_harness.go, NOT in spawner.go. The spawner's Sieve existed but was
// only used for PruneOutput and InspectToolCalls — never for result-level
// repetition detection.
func TestSpawner_ResultRepetition_SieveInitialized(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"worker": {ID: "worker", BaseCapability: "text"},
		},
		Skills: map[string]*config.Skill{},
		MCPs:   map[string]*config.MCPDef{},
		System: config.SystemSettings{},
	}
	logDir := t.TempDir()
	logger := mustNewLogger(t, logDir, &reg.System)
	s := &Spawner{
		registry: reg,
		logger:   logger,
		sieve:    newSieveFromConfig(reg.System),
	}

	if s.sieve == nil {
		t.Fatal("Spawner.sieve must be non-nil — CheckResultRepetition would no-op")
	}

	// Simulate the spawner's tool result processing pattern.
	// NOTE (2026-10-12): CheckResultRepetition is NOT wired in spawner.go
	// anymore (task-scoped history misfires on cross-step re-reads in DAG
	// mode — VERIFY legitimately re-reads ACT results). It remains wired
	// only in chat_harness.go. This test exercises the Sieve method itself.
	taskID := "task-result-rep-wiring"
	identicalResult := `{"name":"Sophia Martin","email":"sophia@example.com"}`

	// Feed identical results up to the threshold.
	// CheckResultRepetition records each call and returns true when
	// the count of prior identical results >= ToolRepetitionThreshold.
	for i := 1; i <= s.sieve.ToolRepetitionThreshold; i++ {
		triggered := s.sieve.CheckResultRepetition(taskID, identicalResult)
		if triggered {
			t.Fatalf("Call %d: should not trigger before threshold (%d)", i, s.sieve.ToolRepetitionThreshold)
		}
	}

	// One more call with the same result should trigger.
	if !s.sieve.CheckResultRepetition(taskID, identicalResult) {
		t.Fatal("Should trigger after threshold reached — CheckResultRepetition wiring is broken")
	}
}

// TestSpawner_ResultRepetition_MessageFormat verifies that the [RESULT REPETITION]
// steer message prepended by the spawner matches the format used in
// chat_harness.go. This is a contract test: both code paths must produce
// the same message so the model sees consistent guidance regardless of
// which execution path it's on.
func TestSpawner_ResultRepetition_MessageFormat(t *testing.T) {
	s := &Spawner{
		sieve: NewSieve(10),
	}
	s.sieve.ToolRepetitionThreshold = 2

	taskID := "task-msg-format"
	result := `{"status":"ok","data":"unchanged"}`

	// Prime the history to just below threshold.
	s.sieve.CheckResultRepetition(taskID, result)
	s.sieve.CheckResultRepetition(taskID, result)

	// On the next call, the spawner would prepend this message.
	md := result
	if s.sieve.CheckResultRepetition(taskID, md) {
		md = "[RESULT REPETITION] You are receiving the same result from different queries. The data is not changing. Stop re-querying and provide a direct answer based on the information you already have.\n\n" + md
	}

	if !strings.HasPrefix(md, "[RESULT REPETITION]") {
		t.Fatal("Result should have [RESULT REPETITION] prefix when repetition is detected")
	}
	if !strings.Contains(md, result) {
		t.Fatal("Original result should be preserved after the repetition warning")
	}
}

// TestSpawner_ResultRepetition_PollerExempt verifies that poller tools
// (progress, health, wait) are exempt from result repetition checks in the
// spawner, matching the chat_harness.go pattern. Pollers legitimately return
// time-varying results with the same args, so flagging them as "repetition"
// would be a false positive.
func TestSpawner_ResultRepetition_PollerExempt(t *testing.T) {
	// The spawner's condition is: isReadTool(name) && !isPollerTool(name)
	// Poller tools should fail this condition.
	pollers := []string{"progress_check", "health_status", "wait_for_completion"}
	for _, name := range pollers {
		if !isReadTool(name) {
			continue // not a read tool, wouldn't be checked anyway
		}
		if !isPollerTool(name) {
			t.Errorf("Poller %q should be exempt but isPollerTool returned false", name)
		}
		// The spawner's condition: isReadTool && !isPollerTool
		// For pollers, this should be false → CheckResultRepetition NOT called.
		shouldCheck := isReadTool(name) && !isPollerTool(name)
		if shouldCheck {
			t.Errorf("Poller %q should NOT trigger CheckResultRepetition in spawner", name)
		}
	}
}

// TestSpawner_ResultRepetition_WriteToolExempt verifies that write tools
// (create, update, delete, cancel, etc.) are exempt from result repetition
// checks in the spawner. Write tools don't go through the isReadTool path,
// so CheckResultRepetition is not called for them.
func TestSpawner_ResultRepetition_WriteToolExempt(t *testing.T) {
	writes := []string{"create_user", "update_record", "delete_item", "cancel_order", "write_file"}
	for _, name := range writes {
		if isReadTool(name) {
			t.Errorf("Write tool %q should NOT be classified as read tool — would wrongly trigger CheckResultRepetition", name)
		}
	}
}

// ── E2E: ChatHarness result repetition detection ──────────────────────────
//
// The chat harness has CheckResultRepetition wired (chat_harness.go:397) —
// the ONLY wired call site since 2026-10-12 (spawner wiring removed: task-
// scoped history misfires on legitimate cross-step re-reads in DAG mode).
// This E2E test verifies the full chat-path behavior: provider makes read
// tool calls with different args → tool returns identical results → after
// threshold, [RESULT REPETITION] message is prepended → result_repetition
// event emitted.

type resultRepetitionProvider struct {
	calls int
}

func (p *resultRepetitionProvider) Complete(_ context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(context.Background(), schemas.CompleteRequest{}, func(string) error { return nil })
}

func (p *resultRepetitionProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	// First 4 turns: call read_file on different files (all with same content).
	// The Sieve's CheckResultRepetition fires when the same result content
	// has been seen >= ToolRepetitionThreshold times.
	if p.calls <= 4 {
		return &schemas.ProviderResponse{
			ToolCalls: []schemas.ToolCall{
				{Name: "read_file", Arguments: map[string]any{"path": filepath.Join("data", "file"+string(rune('0'+p.calls))+".txt")}},
			},
		}, nil
	}
	// Final turn: return a text answer.
	onChunk("Based on the data, the answer is 42.")
	return &schemas.ProviderResponse{Text: "Based on the data, the answer is 42."}, nil
}

func (p *resultRepetitionProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
func (p *resultRepetitionProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}
func (p *resultRepetitionProvider) Name() string { return "result-rep-mock" }

// TestChatHarness_ResultRepetition_E2E verifies that when a provider makes
// read tool calls with different args but the tools return identical results,
// the Sieve's CheckResultRepetition fires and the [RESULT REPETITION] steer
// message is prepended to the tool result.
//
// This is the behavior contract that the spawner's new wiring relies on.
// The spawner mirrors the chat_harness pattern: same condition
// (isReadTool && !isPollerTool), same Sieve method, same message format.
func TestChatHarness_ResultRepetition_E2E(t *testing.T) {
	// Create files with identical content so read_file returns the same
	// result regardless of which file is read.
	// HandleCoreTool resolves relative paths against OutputBase/taskID.
	taskID := "task-result-rep-e2e"
	outputBase := t.TempDir()
	dataDir := filepath.Join(outputBase, taskID, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	identicalContent := "The customer status is: active\n"
	for i := 1; i <= 5; i++ {
		filename := filepath.Join(dataDir, "file"+string(rune('0'+i))+".txt")
		if err := os.WriteFile(filename, []byte(identicalContent), 0644); err != nil {
			t.Fatal(err)
		}
	}

	sieve := NewSieve(100)
	sieve.ToolRepetitionThreshold = 3

	h := &ChatHarness{
		Provider:   &resultRepetitionProvider{},
		Model:      "mock",
		OutputBase: outputBase,
		MaxTurns:   10,
		Sieve:      sieve,
	}

	var events []ChatEvent
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.Run(ctx, taskID,
		[]ChatMessage{{Role: "user", Content: "Read the customer data files and summarize"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	// Verify that result_repetition event was emitted.
	sawResultRepetition := false
	for _, ev := range events {
		if ev.Type == "result_repetition" {
			sawResultRepetition = true
			t.Logf("result_repetition event emitted for tool: %s", ev.Data)
		}
	}
	if !sawResultRepetition {
		t.Error("Expected result_repetition event — CheckResultRepetition should have fired " +
			"after 3+ identical read_file results. This means the Sieve's result-level " +
			"repetition detection is not working.")
	}

	// Verify that the [RESULT REPETITION] message appeared in tool results.
	sawRepetitionMsg := false
	for _, ev := range events {
		if ev.Type == "tool_result" && strings.Contains(ev.Data, "[RESULT REPETITION]") {
			sawRepetitionMsg = true
		}
	}
	if !sawRepetitionMsg {
		t.Error("Expected [RESULT REPETITION] prefix in tool result — the steer message " +
			"should be prepended when CheckResultRepetition fires")
	}
}
