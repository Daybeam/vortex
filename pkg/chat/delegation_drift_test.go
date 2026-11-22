package chat

import (
	"context"
	"fmt"
	"testing"

	"github.com/daybeam/vortex/core/sieve"
	"github.com/daybeam/vortex/schemas"
)

// readLoopProvider returns one read_file call per turn for N turns,
// then a done response. Used to test delegation drift detection.
type readLoopProvider struct {
	calls     int
	readCalls int
}

func (m *readLoopProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *readLoopProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	if m.calls <= m.readCalls {
		onChunk("Reading a file.")
		return &schemas.ProviderResponse{
			Text: "Reading a file.",
			ToolCalls: []schemas.ToolCall{
				{Name: "read_file", Arguments: map[string]any{"path": fmt.Sprintf("file%d.txt", m.calls)}},
			},
		}, nil
	}
	onChunk("Done.")
	return &schemas.ProviderResponse{Text: "Done."}, nil
}

func (m *readLoopProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *readLoopProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *readLoopProvider) Name() string { return "readloop" }

// writeLoopProvider returns one write_file call per turn for N turns,
// then a done response. Used to verify non-read tools don't trigger drift.
type writeLoopProvider struct {
	calls      int
	writeCalls int
}

func (m *writeLoopProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *writeLoopProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	if m.calls <= m.writeCalls {
		onChunk("Writing a file.")
		return &schemas.ProviderResponse{
			Text: "Writing a file.",
			ToolCalls: []schemas.ToolCall{
				{Name: "write_file", Arguments: map[string]any{"path": fmt.Sprintf("file%d.txt", m.calls), "content": "data"}},
			},
		}, nil
	}
	onChunk("Done.")
	return &schemas.ProviderResponse{Text: "Done."}, nil
}

func (m *writeLoopProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *writeLoopProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *writeLoopProvider) Name() string { return "writeloop" }

// grepLoopProvider returns one grep call per turn for N turns,
// then a done response. Used to verify grep triggers drift.
type grepLoopProvider struct {
	calls     int
	grepCalls int
}

func (m *grepLoopProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return m.StreamComplete(ctx, req, func(string) error { return nil })
}

func (m *grepLoopProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	m.calls++
	if m.calls <= m.grepCalls {
		onChunk("Searching.")
		return &schemas.ProviderResponse{
			Text: "Searching.",
			ToolCalls: []schemas.ToolCall{
				{Name: "grep", Arguments: map[string]any{"pattern": "test", "path": "."}},
			},
		}, nil
	}
	onChunk("Done.")
	return &schemas.ProviderResponse{Text: "Done."}, nil
}

func (m *grepLoopProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, nil
}
func (m *grepLoopProvider) CountTokens(ctx context.Context, text string) (int, error) {
	return 0, nil
}
func (m *grepLoopProvider) Name() string { return "greploop" }

func countDelegationDriftEvents(events []ChatEvent) int {
	count := 0
	for _, ev := range events {
		if ev.Type == "delegation_drift" {
			count++
		}
	}
	return count
}

// TestDelegationDrift_FiresAtThreshold verifies that the delegation drift
// steer fires when the model makes 6 or more read_file calls in a single
// turn (Run call).
func TestDelegationDrift_FiresAtThreshold(t *testing.T) {
	h := &ChatHarness{
		Provider:   &readLoopProvider{readCalls: 6},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   10,
		Sieve:      sieve.NewSieve(100),
	}
	var events []ChatEvent
	h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "audit all files"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if drifts := countDelegationDriftEvents(events); drifts != 1 {
		t.Errorf("expected 1 delegation_drift event at threshold 6; got %d", drifts)
	}
}

// TestDelegationDrift_DoesNotFireBelowThreshold verifies that 5 read_file
// calls (below the threshold of 6) do not trigger the drift steer.
func TestDelegationDrift_DoesNotFireBelowThreshold(t *testing.T) {
	h := &ChatHarness{
		Provider:   &readLoopProvider{readCalls: 5},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   10,
		Sieve:      sieve.NewSieve(100),
	}
	var events []ChatEvent
	h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "check a few files"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if drifts := countDelegationDriftEvents(events); drifts != 0 {
		t.Errorf("expected 0 delegation_drift events below threshold; got %d", drifts)
	}
}

// TestDelegationDrift_FiresOnlyOnce verifies that the delegation drift
// steer fires at most once per turn, even if the model continues making
// read_file calls after the threshold is reached.
func TestDelegationDrift_FiresOnlyOnce(t *testing.T) {
	h := &ChatHarness{
		Provider:   &readLoopProvider{readCalls: 10},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   15,
		Sieve:      sieve.NewSieve(100),
	}
	var events []ChatEvent
	h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "audit all files"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if drifts := countDelegationDriftEvents(events); drifts != 1 {
		t.Errorf("expected 1 delegation_drift event (latched once per turn); got %d", drifts)
	}
}

// TestDelegationDrift_NotForNonReadTools verifies that write_file calls
// do not trigger the delegation drift steer.
func TestDelegationDrift_NotForNonReadTools(t *testing.T) {
	h := &ChatHarness{
		Provider:   &writeLoopProvider{writeCalls: 8},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   12,
		Sieve:      sieve.NewSieve(100),
	}
	var events []ChatEvent
	h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "write files"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if drifts := countDelegationDriftEvents(events); drifts != 0 {
		t.Errorf("expected 0 delegation_drift events for write tools; got %d", drifts)
	}
}

// TestDelegationDrift_GrepTriggersDrift verifies that grep calls also
// trigger the delegation drift detection, not just read_file.
func TestDelegationDrift_GrepTriggersDrift(t *testing.T) {
	h := &ChatHarness{
		Provider:   &grepLoopProvider{grepCalls: 7},
		Model:      "mock",
		OutputBase: t.TempDir(),
		MaxTurns:   12,
		Sieve:      sieve.NewSieve(100),
	}
	var events []ChatEvent
	h.Run(context.Background(), "task1",
		[]ChatMessage{{Role: "user", Content: "search for patterns"}},
		func(ev ChatEvent) error { events = append(events, ev); return nil })

	if drifts := countDelegationDriftEvents(events); drifts != 1 {
		t.Errorf("expected 1 delegation_drift event for grep at threshold; got %d", drifts)
	}
}
