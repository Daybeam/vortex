package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// harness-bench D3 (2026-10-30).
//
// A chat-delegated task ran with WorkspaceRoot="", so scheduler_dag_exec passed
// SessionRoot="" into SpawnRequest, the spawned step agent built its
// SafePathValidator with an empty SessionRoot, and its core tools resolved
// relative paths against outputs/<taskID> — a directory containing only
// manifest.json. The delegated agent could still write files, so nothing
// errored; it simply could not read the documents it had been asked about.
// In a transcript that is indistinguishable from "the model delegated badly",
// which is why the first read of the 0/41 measurement was wrong.
//
// These tests pin the two halves of the fix:
//
//	TestSubmitWithWorkspace_CarriesRootIntoGraph — the root reaches the graph,
//	  which is where scheduler_dag_exec.go:339 reads it from.
//	TestSubmitWithSession_KeepsEmptyRoot — the legacy entry point is unchanged,
//	  so non-chat callers keep their per-task directories.
//	TestSubmitWithWorkspace_*Binding — the session binding that a non-empty root
//	  now triggers: permissive whitelist accepts, a whitelist that excludes the
//	  root rejects loudly (rather than silently falling back to outputs/).

func submitTestStep() []schemas.StepInput {
	return []schemas.StepInput{{ID: "s1", Task: "Read report.md and write review_findings.json"}}
}

func TestSubmitWithWorkspace_CarriesRootIntoGraph(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	ws := t.TempDir()
	id, err := s.SubmitWithWorkspace(submitTestStep(), "sess-d3", ws)
	if err != nil {
		t.Fatalf("SubmitWithWorkspace: %v", err)
	}

	st, ok := s.GetStatus(id, "full")
	if !ok {
		t.Fatalf("no status for task %s", id)
	}
	got, _ := st["workspace_root"].(string)
	if got != ws {
		t.Fatalf("workspace_root = %q, want %q\n"+
			"An empty root here is the D3 bug: the delegated agent's core tools "+
			"resolve against outputs/<taskID> instead of the workspace.", got, ws)
	}
}

func TestSubmitWithSession_KeepsEmptyRoot(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	id, err := s.SubmitWithSession(submitTestStep(), "sess-legacy")
	if err != nil {
		t.Fatalf("SubmitWithSession: %v", err)
	}

	st, ok := s.GetStatus(id, "full")
	if !ok {
		t.Fatalf("no status for task %s", id)
	}
	if got, _ := st["workspace_root"].(string); got != "" {
		t.Fatalf("workspace_root = %q, want empty (legacy callers must keep "+
			"per-task output directories)", got)
	}
}

func TestSubmitWithWorkspace_BindsSessionWhenWhitelistIsPermissive(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	// Eval containers ship "sandbox": {} — an empty whitelist means permissive.
	s.Sessions = NewSessionManager(nil, 0)

	ws := t.TempDir()
	if _, err := s.SubmitWithWorkspace(submitTestStep(), "sess-perm", ws); err != nil {
		t.Fatalf("SubmitWithWorkspace under a permissive whitelist: %v", err)
	}
	sc := s.Sessions.Lookup("sess-perm")
	if sc == nil {
		t.Fatal("session was not bound; delegated tasks would not find it")
	}
	if sc.Workspace == "" {
		t.Fatal("session bound to an empty workspace")
	}
}

func TestSubmitWithWorkspace_RejectsRootOutsideWhitelist(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	s.Sessions = NewSessionManager([]string{t.TempDir()}, 0)

	// Not in the whitelist. The rejection has to be an error: silently
	// dropping the root would put us back in D3 with no trace in the logs.
	_, err := s.SubmitWithWorkspace(submitTestStep(), "sess-deny", t.TempDir())
	if err == nil {
		t.Fatal("expected a rejection when the workspace root is outside the whitelist")
	}
	if !strings.Contains(err.Error(), "session-workspace binding rejected") {
		t.Fatalf("unexpected error, want a binding rejection: %v", err)
	}
}
