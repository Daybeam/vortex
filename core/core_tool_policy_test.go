package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
)

// ─── Why these tests exist ────────────────────────────────────────────────
//
// The planner is supposed to produce a plan, never to execute it. Until now
// nothing enforced that: buildMCPServers attached the _core server to every
// spawn, and processToolCalls dispatched any CoreToolNames match without
// looking at the role. Role.CoreTools closes both ends, and the two halves are
// separately testable — the schema half here, the dispatch half through
// coreToolPolicy.allows (the exact predicate processToolCalls consults).

func roleWithCoreTools(id string, coreTools []string) *config.Role {
	return &config.Role{ID: id, Name: id, CoreTools: coreTools}
}

// TestCoreToolPolicy_NilMeansUnrestricted pins backward compatibility: every
// config written before this field existed must behave exactly as before.
func TestCoreToolPolicy_NilMeansUnrestricted(t *testing.T) {
	reg := newTestRegistry(roleWithCoreTools("executor", nil))
	s := &Spawner{registry: reg}

	p := s.coreToolPolicyFor("executor")
	if p.restricted {
		t.Fatal("nil core_tools must mean unrestricted")
	}
	for name := range CoreToolNames {
		if !p.allows(name) {
			t.Fatalf("%s must be allowed for an unrestricted role", name)
		}
	}
	if got := len(p.filter(CoreToolDefinitions())); got != len(CoreToolDefinitions()) {
		t.Fatalf("filter dropped tools for an unrestricted role: %d", got)
	}
}

// TestCoreToolPolicy_EmptySliceDeniesEverything is the planner's case: it must
// not be able to run code, write files, or read arbitrary paths while planning.
func TestCoreToolPolicy_EmptySliceDeniesEverything(t *testing.T) {
	reg := newTestRegistry(roleWithCoreTools("task_planner", []string{}))
	s := &Spawner{registry: reg}

	p := s.coreToolPolicyFor("task_planner")
	if !p.restricted {
		t.Fatal("an empty (non-nil) core_tools must mean restricted")
	}
	if p.allows("execute_code") {
		t.Fatal("task_planner must not be allowed execute_code")
	}
	if got := len(p.filter(CoreToolDefinitions())); got != 0 {
		t.Fatalf("expected no core tool definitions, got %d", got)
	}
}

// TestCoreToolPolicy_SubsetKeepsOnlyNamed guards the middle case and the order
// of the surviving definitions (prompt stability depends on it).
func TestCoreToolPolicy_SubsetKeepsOnlyNamed(t *testing.T) {
	reg := newTestRegistry(roleWithCoreTools("reader", []string{"read_file"}))
	s := &Spawner{registry: reg}

	p := s.coreToolPolicyFor("reader")
	if !p.allows("read_file") {
		t.Fatal("read_file was declared and must be allowed")
	}
	if p.allows("execute_code") {
		t.Fatal("execute_code was not declared and must be denied")
	}

	var order []string
	for _, d := range p.filter(CoreToolDefinitions()) {
		order = append(order, d.Name)
	}
	if len(order) != 1 || order[0] != "read_file" {
		t.Fatalf("expected exactly [read_file], got %v", order)
	}
}

// TestCoreToolPolicy_UnknownNamesCannotWidenSurface: a typo in config must not
// accidentally grant anything, and must not panic.
func TestCoreToolPolicy_UnknownNamesCannotWidenSurface(t *testing.T) {
	reg := newTestRegistry(roleWithCoreTools("typo", []string{"execute_cod", "rm_rf"}))
	s := &Spawner{registry: reg}

	p := s.coreToolPolicyFor("typo")
	if !p.restricted {
		t.Fatal("a non-nil list must restrict even when every entry is bogus")
	}
	for name := range CoreToolNames {
		if p.allows(name) {
			t.Fatalf("%s must not be allowed by a config that never named it", name)
		}
	}
}

// TestCoreToolPolicy_UnknownRoleIsUnrestricted keeps nil-safety honest: a
// missing role must not silently forbid everything (that would break callers
// that pass a generated role id the registry has not seen).
func TestCoreToolPolicy_UnknownRoleIsUnrestricted(t *testing.T) {
	s := &Spawner{registry: newTestRegistry()}
	if p := s.coreToolPolicyFor("nobody"); p.restricted {
		t.Fatal("unknown role must fall back to unrestricted")
	}
	var nilSpawner *Spawner
	if p := nilSpawner.coreToolPolicyFor("x"); p.restricted {
		t.Fatal("nil spawner must fall back to unrestricted")
	}
}

// TestBuildMCPServers_PlannerGetsNoCoreServer is the assembly half of the
// defence: the planner's prompt must not even advertise execute_code, or the
// instruction "do not execute" is competing with a tool that invites it.
func TestBuildMCPServers_PlannerGetsNoCoreServer(t *testing.T) {
	reg := newTestRegistry(
		roleWithCoreTools("task_planner", []string{}),
		roleWithCoreTools("executor", nil),
	)
	s := &Spawner{registry: reg}

	// nil bindings: no MCPs to walk, which is exactly the planner's shape
	// (task_planner binds nothing).
	planned := s.buildMCPServers(context.Background(), nil, "task_x", nil, "p", "task_planner")
	for _, srv := range planned {
		if srv.Name == "_core" {
			t.Fatal("task_planner must not receive the _core server")
		}
	}

	exec := s.buildMCPServers(context.Background(), nil, "task_x", nil, "p", "executor")
	found := false
	for _, srv := range exec {
		if srv.Name == "_core" {
			found = true
			if len(srv.Tools) != len(CoreToolDefinitions()) {
				t.Fatalf("executor must keep the full core surface, got %d", len(srv.Tools))
			}
		}
	}
	if !found {
		t.Fatal("an unrestricted role must still receive the _core server")
	}
}
