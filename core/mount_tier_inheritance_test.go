package core

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// ── Regression suite for the 2026-10-26 AppWorld wedge ──────────────────────
//
// Failure being pinned here, in order:
//
//  1. An MCP that does not declare `mount_policy` is admin_only
//     (config/mount_policy.go: MCPs default to admin_only, skills to open).
//  2. The mount gate rejects the WHOLE ROLE when one of its bound MCPs is
//     admin_only and the submitter is non-admin, with a message that names only
//     the role — so "one of its MCPs is unclassified" is indistinguishable from
//     "this role is genuinely privileged".
//  3. The engine's own sub-spawns (decider / planner / rubric / step body) built
//     a SpawnRequest without a Hub; prepareSpawn then substituted
//     NewContextHub(reg, nil, …) whose Graph is nil, so the tier silently
//     folded to false and every sub-spawn was judged a PUBLIC caller.
//     ⇒ `decision_required(step_failed)` spawned `s1_decider`, which died in
//     <1 ms with `role "appworld_agent" is not available to non-admin
//     submitters`; the task never reached a terminal status and the harness
//     polled it for the whole 5.5 h arm budget.
//
// The fix: sub-spawns carry the owning graph's tier on the request
// (SpawnRequest.SubmitterIsAdmin), and the gate reads it via
// submitterIsAdminFor. Inherit — never default to admin, never default to
// public.

func tierTestHub(policies map[string]string) *ContextHub {
	role := &config.Role{
		ID: "appworld_agent",
		BoundMCPBindings: []config.MCPBinding{
			{MCPID: "privileged_mcp"},
		},
	}
	mcps := map[string]*config.MCPDef{}
	for id, pol := range policies {
		mcps[id] = &config.MCPDef{ID: id, MountPolicy: pol}
	}
	reg := &config.Registry{
		Roles: map[string]*config.Role{"appworld_agent": role},
		MCPs:  mcps,
	}
	// Graph == nil mirrors exactly what prepareSpawn builds for a Hub-less
	// sub-spawn; that nil is the origin of the whole defect.
	return NewContextHub(reg, nil, nil)
}

// T1: the failure itself — a non-admin submitter naming a role bound to an
// unclassified MCP is refused wholesale.
func TestResolveFinalMCPs_NonAdminBoundUnclassifiedMCPIsRejected(t *testing.T) {
	hub := tierTestHub(map[string]string{"privileged_mcp": ""}) // no mount_policy

	_, err := ResolveFinalMCPs(hub, "appworld_agent", nil, nil, false)
	if err == nil {
		t.Fatal("non-admin + bound MCP without mount_policy must be rejected; " +
			"if this starts passing, the mount default changed and every " +
			"unclassified MCP is now public — a security change, not a fix")
	}
	var rej *MountPolicyRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("expected *MountPolicyRejectedError (the operator log depends on it), got %T: %v", err, err)
	}
	if rej.MCPID != "privileged_mcp" || rej.Policy != config.MountPolicyAdminOnly {
		t.Fatalf("rejection must carry the real reason for the internal log, got mcp=%q policy=%q",
			rej.MCPID, rej.Policy)
	}
	// admin-hiding: the caller-facing message must NOT name the MCP.
	if strings.Contains(err.Error(), "privileged_mcp") {
		t.Fatalf("public message leaks the privileged MCP id: %q", err.Error())
	}
}

// T2: the fix — the same spawn succeeds once the tier is carried on the request
// (which is what an internal sub-spawn now does).
func TestResolveFinalMCPs_InternalSpawnInheritsTier(t *testing.T) {
	hub := tierTestHub(map[string]string{"privileged_mcp": ""})

	req := &SpawnRequest{Hub: hub, TaskID: "t1", StepID: "s1_decider", RoleID: "appworld_agent", SubmitterIsAdmin: true}
	got, err := ResolveFinalMCPs(req.Hub, req.RoleID, nil, nil, submitterIsAdminFor(req))
	if err != nil {
		t.Fatalf("internal sub-spawn carrying the owning graph's admin tier must pass the gate, got %v", err)
	}
	if len(got) != 1 || got[0].MCPID != "privileged_mcp" {
		t.Fatalf("expected the single bound MCP, got %+v", got)
	}
}

// T3: fail-closed — an engine sub-spawn that forgets the tier is still public.
// Without this, "inherit" would degenerate into "everything is admin".
func TestSubmitterIsAdminFor_FailClosedWhenTierMissing(t *testing.T) {
	hub := tierTestHub(map[string]string{"privileged_mcp": ""})

	cases := []struct {
		name string
		req  *SpawnRequest
		want bool
	}{
		{"hub graph nil + no field (the old bug)", &SpawnRequest{Hub: hub}, false},
		{"nil request", nil, false},
		{"hub nil + no field", &SpawnRequest{Hub: nil}, false},
		{"hub graph nil + field set", &SpawnRequest{Hub: hub, SubmitterIsAdmin: true}, true},
	}
	for _, c := range cases {
		if got := submitterIsAdminFor(c.req); got != c.want {
			t.Errorf("%s: submitterIsAdminFor = %v, want %v", c.name, got, c.want)
		}
	}

	// And that false result must actually reject, not merely be reported false.
	req := &SpawnRequest{Hub: hub, RoleID: "appworld_agent"}
	if _, err := ResolveFinalMCPs(req.Hub, req.RoleID, nil, nil, submitterIsAdminFor(req)); err == nil {
		t.Fatal("a sub-spawn with no tier must still be rejected (fail-closed)")
	}
}

// T4: the escape hatch for ordinary business MCPs — declaring open makes the
// role usable at every tier. This is the intended way out of T1, and the reason
// "public can use nothing" is a CONFIG bug rather than an architectural one.
func TestResolveFinalMCPs_OpenPolicyIsUsableByNonAdmin(t *testing.T) {
	hub := tierTestHub(map[string]string{"privileged_mcp": config.MountPolicyOpen})
	if _, err := ResolveFinalMCPs(hub, "appworld_agent", nil, nil, false); err != nil {
		t.Fatalf("mount_policy=open must be usable by a non-admin submitter, got %v", err)
	}
}

// T5 lives in the config package (config/builtin_roles_tier_test.go): it needs
// the unexported `builtinRoles` table, and the point it pins — "the three
// builtin meta roles are admin-only BY DESIGN (AUTH-1), which is a different
// refusal from the mount gate" — belongs next to that table.

// T6: structural guard — the defect was "6 call sites, each of which looked
// fine in isolation". A unit test cannot see a missing field at a call site, so
// this reads the sources and asserts the invariant. If it fails because you
// added a NEW internal spawn, add SubmitterIsAdmin there (inherit the owning
// graph's tier); do not delete the entry.
func TestInternalSpawnSitesCarrySubmitterTier(t *testing.T) {
	sites := []struct{ file, anchor string }{
		{"scheduler_dag_exec.go", "graph.SubmitterIsAdmin"},
		{"scheduler_decision_audit.go", "graph.SubmitterIsAdmin"},
		{"scheduler_planner.go", "SubmitterIsAdmin: submitterIsAdmin"},
		{"scheduler_decision_gate.go", "SubmitterIsAdmin: submitterIsAdmin"},
		{"replay_scheduler.go", "NO SubmitterIsAdmin ON PURPOSE"},
	}
	_, self, _, _ := runtime.Caller(0)
	dir := filepath.Dir(self)
	for _, s := range sites {
		b, err := os.ReadFile(filepath.Join(dir, s.file))
		if err != nil {
			t.Fatalf("read %s: %v", s.file, err)
		}
		if !strings.Contains(string(b), s.anchor) {
			t.Errorf("%s no longer carries %q — an internal sub-spawn lost its "+
				"submitter tier and will silently run as a public caller", s.file, s.anchor)
		}
	}
}
