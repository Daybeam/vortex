package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// registerContractOutput is best-effort audit registration, and every guard in
// it exists because the alternative silently produces a wrong number:
//
//   - a relative contract path with no workspace root resolves against the
//     daemon's cwd, which can register a file the task never produced;
//   - the same (step, path) can already be registered from the model's
//     `output_ref`, and `len(graph.OutputFiles)` is read as the deliverable
//     count;
//   - "declared a deliverable that is absent" must be visible, not silent.

func newRegistrationEngine(t *testing.T) *DirectedEngine {
	t.Helper()
	return &DirectedEngine{
		logger: mustNewLogger(t, mustTempDir(t, "contract_output"), &config.SystemSettings{}),
	}
}

func graphWithContract(workspace, path string) (*schemas.TaskGraph, *schemas.Step) {
	graph := &schemas.TaskGraph{TaskID: "task-1", WorkspaceRoot: workspace}
	step := &schemas.Step{ID: "s1"}
	step.OutputContract.Path = path
	step.OutputContract.Format = "md"
	return graph, step
}

func TestRegisterContractOutput_SkipsEmptyPath(t *testing.T) {
	graph, step := graphWithContract(t.TempDir(), "")
	newRegistrationEngine(t).registerContractOutput(graph, step, graph.TaskID)
	if len(graph.OutputFiles) != 0 {
		t.Fatalf("output_files = %d, want 0 for an undeclared contract", len(graph.OutputFiles))
	}
}

func TestRegisterContractOutput_RefusesRelativePathWithoutWorkspaceRoot(t *testing.T) {
	// The trap: with no workspace root the path would be resolved against the
	// process cwd, where a same-named file exists. Registering it would claim
	// the task produced a file it never touched.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("not this task's"), 0o644); err != nil {
		t.Fatalf("write bait file: %v", err)
	}
	t.Chdir(dir)

	graph, step := graphWithContract("", "report.md")
	newRegistrationEngine(t).registerContractOutput(graph, step, graph.TaskID)
	if len(graph.OutputFiles) != 0 {
		t.Fatalf("registered %q from the process cwd; want refusal when WorkspaceRoot is empty",
			graph.OutputFiles[0].Path)
	}
}

func TestRegisterContractOutput_RegistersExistingFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "report.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write deliverable: %v", err)
	}
	graph, step := graphWithContract(workspace, "report.md")

	newRegistrationEngine(t).registerContractOutput(graph, step, graph.TaskID)

	if len(graph.OutputFiles) != 1 {
		t.Fatalf("output_files = %d, want 1", len(graph.OutputFiles))
	}
	got := graph.OutputFiles[0]
	if got.StepID != "s1" || got.Path != "report.md" {
		t.Fatalf("registered %+v, want step s1 / path report.md", got)
	}
	if got.SizeBytes != len("hello") {
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, len("hello"))
	}
	if got.Format != "md" {
		t.Errorf("Format = %q, want md", got.Format)
	}
}

func TestRegisterContractOutput_DoesNotDoubleRegister(t *testing.T) {
	// A model that does what prompt_assembler.go:669 asks for sets `output_ref`
	// to the same path the contract declares. Both branches then describe one
	// file, and the count is read downstream.
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "report.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write deliverable: %v", err)
	}
	graph, step := graphWithContract(workspace, "report.md")
	graph.OutputFiles = append(graph.OutputFiles, schemas.OutputFile{
		StepID: "s1",
		Path:   "report.md",
	})

	newRegistrationEngine(t).registerContractOutput(graph, step, graph.TaskID)

	if len(graph.OutputFiles) != 1 {
		t.Fatalf("output_files = %d, want 1 (already registered by output_ref)", len(graph.OutputFiles))
	}
}

func TestRegisterContractOutput_MissingFileIsLoggedNotFatal(t *testing.T) {
	graph, step := graphWithContract(t.TempDir(), "absent.md")
	newRegistrationEngine(t).registerContractOutput(graph, step, graph.TaskID)
	if len(graph.OutputFiles) != 0 {
		t.Fatalf("output_files = %d, want 0 for a missing deliverable", len(graph.OutputFiles))
	}
}

func TestContractFileMissingEventNameIsSnakeCase(t *testing.T) {
	// It was passed as a bare string literal ("EventContractFileMissing") until
	// 2026-11-22, so the Go identifier went out as the event name while every
	// other event on the bus was snake_case.
	if got := string(obs.EventContractFileMissing); got != "contract_file_missing" {
		t.Fatalf("event name = %q, want %q", got, "contract_file_missing")
	}
}
