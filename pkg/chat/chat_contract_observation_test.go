package chat

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// Q0-1 option B (2026-11-04): the run's declared/present/missing artifact state
// had to become a recorded metric, not a verdict. Before this, [chat-turn-summary]
// carried contract_declared= only — the count of what the prompt promised — while
// present/missing were computed twice for *other* purposes (the steer at
// chat_harness.go:1087 and the delegation receipt at chat_harness_status.go:118)
// and never recorded. So the eval could see how many files were promised but not
// how many arrived, which is the number option B exists to report.

const contractObservationPrompt = "Produce the report.\n\n" +
	"## Enforced workspace change contract\n\n" +
	"- Create: `report.md`, `summary.json`\n" +
	"- Modify: none\n" +
	"- Delete: none\n\n" +
	"Create `summary.json` containing:\n\n" +
	"- `total`\n"

func observationHarness(t *testing.T, root string) *ChatHarness {
	t.Helper()
	h := &ChatHarness{SessionRoot: root}
	h.declaredArtifacts = core.ParseDeclaredArtifacts(contractObservationPrompt)
	h.declaredFields = core.ParseDeclaredArtifactFields(contractObservationPrompt)
	if len(h.declaredArtifacts) != 2 {
		t.Fatalf("setup: prompt must declare 2 artifacts, parsed %v", h.declaredArtifacts)
	}
	if len(h.declaredFields["summary.json"]) != 1 {
		t.Fatalf("setup: summary.json must declare 1 field, got %v", h.declaredFields)
	}
	return h
}

func obsWriteArtifact(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestContractObservation_CountsPresentMissingAndShape pins the three counts
// against each other, which is the whole point of recording them together: this
// run delivered every file the contract promised, so a present/missing-only
// metric would have reported it clean. summary.json is valid JSON but not the
// shape the prompt asked for — the failure mode chat_harness.go's declaredFields
// comment records as "existence called it done".
func TestContractObservation_CountsPresentMissingAndShape(t *testing.T) {
	root := t.TempDir()
	obsWriteArtifact(t, root, "report.md", "hello\n")
	obsWriteArtifact(t, root, "summary.json", `{"count": 3}`)

	got := observationHarness(t, root).contractObservation()
	want := "contract_present=2 contract_missing=0 contract_defects=1"
	if got != want {
		t.Errorf("contractObservation() = %q, want %q", got, want)
	}
}

// TestContractObservation_CountsAnAbsentArtifactWithoutAFieldDefect covers the
// other axis: a file that was never written is a missing artifact, not a field
// defect — WorkspaceArtifactDefects skips absent files, so the two counts must
// not double-report the same failure.
func TestContractObservation_CountsAnAbsentArtifactWithoutAFieldDefect(t *testing.T) {
	root := t.TempDir()
	obsWriteArtifact(t, root, "report.md", "hello\n")

	got := observationHarness(t, root).contractObservation()
	want := "contract_present=1 contract_missing=1 contract_defects=0"
	if got != want {
		t.Errorf("contractObservation() = %q, want %q", got, want)
	}
}

func TestContractObservation_ReportsBothArtifactsWhenAllClean(t *testing.T) {
	root := t.TempDir()
	obsWriteArtifact(t, root, "report.md", "hello\n")
	obsWriteArtifact(t, root, "summary.json", `{"total": 3}`)

	got := observationHarness(t, root).contractObservation()
	want := "contract_present=2 contract_missing=0 contract_defects=0"
	if got != want {
		t.Errorf("contractObservation() = %q, want %q", got, want)
	}
}

// TestContractObservation_LegacyLayoutAndNoContractAreNotZero is the half that
// keeps the metric from being read as a score: present=0 is a statement about the
// workspace, and in the legacy outputs/<taskID> layout (SessionRoot == "") the
// declared bare names resolve against the process working directory, which
// measured as present=0 forever — or, on a name collision, as "all artifacts
// exist" over an empty workspace.
func TestContractObservation_LegacyLayoutAndNoContractAreNotZero(t *testing.T) {
	root := t.TempDir()
	obsWriteArtifact(t, root, "report.md", "hello\n")
	obsWriteArtifact(t, root, "summary.json", `{"total": 3}`)

	legacy := observationHarness(t, "")
	if got, want := legacy.contractObservation(), "contract_present=na contract_missing=na contract_defects=na"; got != want {
		t.Errorf("legacy layout: got %q, want %q", got, want)
	}

	noContract := &ChatHarness{SessionRoot: root}
	if got, want := noContract.contractObservation(), "contract_present=na contract_missing=na contract_defects=na"; got != want {
		t.Errorf("no declared contract: got %q, want %q", got, want)
	}
}

// summaryOnlyProvider answers with text on the first turn, so Run exits through
// its normal completion path and the deferred summary line is the one under test.
type summaryOnlyProvider struct{ calls int }

func (p *summaryOnlyProvider) Complete(_ context.Context, _ schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(context.Background(), schemas.CompleteRequest{}, func(string) error { return nil })
}

func (p *summaryOnlyProvider) StreamComplete(_ context.Context, _ schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.calls++
	onChunk("Report written.")
	return &schemas.ProviderResponse{Text: "Report written."}, nil
}

func (p *summaryOnlyProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
func (p *summaryOnlyProvider) CountTokens(_ context.Context, text string) (int, error) {
	return len(text) / 4, nil
}
func (p *summaryOnlyProvider) Name() string { return "contract-observation-mock" }

func TestChatTurnSummary_CarriesContractObservation(t *testing.T) {
	root := t.TempDir()
	obsWriteArtifact(t, root, "report.md", "hello\n")

	var logs bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prevOut)

	h := &ChatHarness{
		Provider:    &summaryOnlyProvider{},
		Model:       "mock",
		OutputBase:  t.TempDir(),
		MaxTurns:    3,
		SessionRoot: root,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = h.Run(ctx, "task-contract-obs", []ChatMessage{{Role: "user", Content: contractObservationPrompt}},
		func(ChatEvent) error { return nil })

	line := ""
	for _, candidate := range strings.Split(logs.String(), "\n") {
		if strings.Contains(candidate, "[chat-turn-summary] task=task-contract-obs") {
			line = candidate
		}
	}
	if line == "" {
		t.Fatal("no [chat-turn-summary] line emitted for the run — the deferred metric did not fire")
	}
	// Existing keys must keep their names and order; only the new state rides on
	// the end of the line.
	for _, want := range []string{
		"contract_declared=2",
		"contract_steer=",
		"contract_defect_steer=0",
		"fields_declared=1",
		"contract_present=1 contract_missing=1 contract_defects=0",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("summary line missing %q.\nline: %s", want, line)
		}
	}
}
