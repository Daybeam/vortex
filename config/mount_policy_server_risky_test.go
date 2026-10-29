package config

import (
	"bytes"
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ── ServerRiskyMCP: the multi-tenant dividing line (2026-10-26) ─────────────
//
// Operator rule: public tenants run as tenants and may do their own thing;
// anything that can reach the SERVER stays admin-managed. The two ways an MCP
// definition reaches the host are an unsandboxed local process and a URL aimed
// at loopback/private space. Everything else is tenant-scoped risk (the tenant
// could already curl it), so gating it would break the closed loop.

func TestServerRiskyMCP(t *testing.T) {
	cases := []struct {
		name string
		def  *MCPDef
		want bool
	}{
		{"nil definition is risky", nil, true},
		{"public https URL is tenant-scoped", &MCPDef{URL: "https://api.example.com/mcp"}, false},
		{"scheme-less public host is tenant-scoped", &MCPDef{URL: "api.example.com:443"}, false},
		{"loopback v4", &MCPDef{URL: "http://127.0.0.1:9200"}, true},
		{"localhost spelling", &MCPDef{URL: "http://localhost:8080"}, true},
		{"cloud metadata (link-local)", &MCPDef{URL: "http://169.254.169.254/latest/meta-data/"}, true},
		{"RFC1918 10/8", &MCPDef{URL: "http://10.1.2.3:3000"}, true},
		{"RFC1918 192.168/16", &MCPDef{URL: "http://192.168.1.10:3000"}, true},
		{"IPv6 loopback bracketed", &MCPDef{URL: "http://[::1]:7777"}, true},
		{"IPv6 ULA", &MCPDef{URL: "http://[fd00::1]:7777"}, true},
		{"unsandboxed local process", &MCPDef{Command: "bash", Args: []string{"-c", "id"}}, true},
		{"sandboxed local process", &MCPDef{Command: "python", Sandboxed: true}, false},
		{"embedded JIT marker, sandboxed", &MCPDef{Command: "__embedded__:js", Sandboxed: true}, false},
		{"non-JSON scheme is not a host", &MCPDef{URL: "unix:///tmp/sock"}, false},

		// Third axis: whose identity does the remote see? A public URL is
		// host-safe but NOT identity-safe when it carries an env credential —
		// the remote then sees the operator, whoever called.
		{"public URL + api_key_env", &MCPDef{URL: "https://gmailmcp.googleapis.com/mcp/v1", APIKeyEnv: "GMAIL_TOKEN"}, true},
		{"public URL + url_auth_placeholder", &MCPDef{URL: "https://api.example.com/mcp?key=__K__", URLAuthPlaceholder: "__K__", APIKeyEnv: "X"}, true},
		{"public URL, no credential", &MCPDef{URL: "https://api.example.com/mcp"}, false},
		// auth_header_name without api_key_env attaches NOTHING at runtime
		// (core/mcp_manager.go errors out instead), so it is a config bug, not
		// an impersonation path.
		{"public URL + auth_header_name only", &MCPDef{URL: "https://api.example.com/mcp", AuthHeaderName: "Authorization"}, false},

		// L1: credential_scope=per_tenant declares that the credential is the
		// tenant's, not the operator's. A public URL + api_key_env + per_tenant
		// is NOT server-risky: the remote sees the tenant, so the risk is
		// tenant-scoped (the tenant can already curl their own account).
		{"per_tenant + api_key_env → not server-risky", &MCPDef{URL: "https://api.example.com/mcp", APIKeyEnv: "TENANT_TOKEN", CredentialScope: CredentialPerTenant}, false},
		{"per_tenant + url_auth_placeholder → not server-risky", &MCPDef{URL: "https://api.example.com/mcp?key=__K__", URLAuthPlaceholder: "__K__", APIKeyEnv: "X", CredentialScope: CredentialPerTenant}, false},
		// Explicit operator scope: same as default (backward compat).
		{"explicit operator + api_key_env → server-risky", &MCPDef{URL: "https://api.example.com/mcp", APIKeyEnv: "TENANT_TOKEN", CredentialScope: CredentialOperator}, true},
		// Unknown scope ⇒ fail-closed ⇒ operator ⇒ server-risky.
		{"unknown scope + api_key_env → fail-closed operator", &MCPDef{URL: "https://api.example.com/mcp", APIKeyEnv: "X", CredentialScope: "garbage"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ServerRiskyMCP(c.def); got != c.want {
				t.Fatalf("ServerRiskyMCP(%+v) = %v, want %v", c.def, got, c.want)
			}
		})
	}
}

// The platform-override bypass: the spawner executes ResolveForPlatform(), so a
// definition that hides its command in `platform_overrides.<goos>.command` must
// be judged on THAT, not on the (empty) base field.
func TestServerRiskyMCP_SeesPlatformOverrideCommand(t *testing.T) {
	def := &MCPDef{
		ID:  "sneaky",
		URL: "https://api.example.com/mcp", // looks safe in the base definition
		PlatformOverrides: map[string]MCPPlatformOverride{
			runtime.GOOS: {Command: "bash", Args: []string{"-c", "id"}},
		},
	}
	if !ServerRiskyMCP(def) {
		t.Fatal("a command injected via platform_overrides must count as a local process; " +
			"reading only MCPDef.Command is a one-field bypass")
	}
	// Same definition, sandboxed ⇒ not server-risky (the sandbox is what makes
	// it acceptable for a tenant to run).
	def.Sandboxed = true
	if ServerRiskyMCP(def) {
		t.Fatal("a sandboxed platform command must not be flagged")
	}
}

// ── L1: CredentialScope — whose identity does the remote see? ───────────────
//
// OAUTH §4 L1: credential_scope makes the credential ownership explicit.
// Default (empty) ⇒ operator (fail-closed). per_tenant ⇒ the tenant's own
// credential, so CarriesOperatorCredential is false even when api_key_env is
// set. See docs/plan/OAUTH_SHARED_MCP_MULTI_TENANT_DESIGN.md §4 L1, §6 O5.

func TestEffectiveCredentialScope(t *testing.T) {
	cases := []struct {
		name string
		def  *MCPDef
		want CredentialScope
	}{
		{"nil ⇒ operator (fail-closed)", nil, CredentialOperator},
		{"empty ⇒ operator (fail-closed)", &MCPDef{ID: "x"}, CredentialOperator},
		{"explicit operator", &MCPDef{ID: "x", CredentialScope: CredentialOperator}, CredentialOperator},
		{"per_tenant", &MCPDef{ID: "x", CredentialScope: CredentialPerTenant}, CredentialPerTenant},
		{"unknown ⇒ operator (fail-closed)", &MCPDef{ID: "x", CredentialScope: "garbage"}, CredentialOperator},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.def.EffectiveCredentialScope(); got != c.want {
				t.Fatalf("EffectiveCredentialScope() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCarriesOperatorCredential_PerTenantIsFalse(t *testing.T) {
	// O5 acceptance: credential_scope=per_tenant + api_key_env ⇒ NOT operator
	// credential. This is the core of L1: the same config that was
	// impersonation yesterday is tenant-scoped today, just by adding one field.
	def := &MCPDef{
		URL:             "https://api.example.com/mcp",
		APIKeyEnv:       "TENANT_TOKEN",
		CredentialScope: CredentialPerTenant,
	}
	if def.CarriesOperatorCredential() {
		t.Fatal("per_tenant scope must not carry operator credential, " +
			"even when api_key_env is set")
	}
}

func TestCarriesOperatorCredential_OperatorDefaultIsTrue(t *testing.T) {
	// Backward compat: no credential_scope ⇒ operator ⇒ carries operator
	// credential when api_key_env is set. Every existing MCP behaves identically.
	def := &MCPDef{
		URL:       "https://api.example.com/mcp",
		APIKeyEnv: "OPS_KEY",
	}
	if !def.CarriesOperatorCredential() {
		t.Fatal("default (empty) scope + api_key_env must carry operator credential " +
			"(backward compat)")
	}
}

func TestCarriesOperatorCredential_NoCredentialIsFalse(t *testing.T) {
	// No api_key_env / url_auth_placeholder ⇒ no credential at all, regardless
	// of scope. A public API needs no auth.
	for _, scope := range []CredentialScope{"", CredentialOperator, CredentialPerTenant} {
		def := &MCPDef{URL: "https://api.example.com/mcp", CredentialScope: scope}
		if def.CarriesOperatorCredential() {
			t.Fatalf("scope=%q: no credential fields set ⇒ must not carry operator credential", scope)
		}
	}
}

// ── MountPolicyHint: ONE line, not a dump ──────────────────────────────────
//
// Operator decision: don't list the unconfigured MCPs (with 20+ every boot
// prints 20 lines nobody reads) — state the rule once.

func TestMountPolicyHintIsOneLine(t *testing.T) {
	r := &Registry{MCPs: map[string]*MCPDef{
		"a": {ID: "a"},                                   // unset
		"b": {ID: "b"},                                   // unset
		"c": {ID: "c", MountPolicy: MountPolicyOpen},     // published
		"d": {ID: "d", MountPolicy: MountPolicyApproval}, // published
	}}
	hint := r.MountPolicyHint()
	if hint == "" {
		t.Fatal("2/4 unset MCPs must produce a hint, otherwise operators never learn the rule")
	}
	if strings.Count(hint, "\n") != 0 {
		t.Fatalf("hint must be a single line, got %q", hint)
	}
	if !strings.Contains(hint, "2/4") {
		t.Fatalf("hint must state the count (2/4), got %q", hint)
	}
	// The rule, not the list: naming individual MCPs is exactly what was ruled out.
	for _, id := range []string{"a", "b"} {
		if strings.Contains(hint, `"`+id+`"`) {
			t.Fatalf("hint lists a specific MCP id (%q): %q", id, hint)
		}
	}
}

func TestMountPolicyHintSilentWhenAllDeclared(t *testing.T) {
	r := &Registry{MCPs: map[string]*MCPDef{
		"c": {ID: "c", MountPolicy: MountPolicyOpen},
	}}
	if hint := r.MountPolicyHint(); hint != "" {
		t.Fatalf("nothing to warn about, expected no output, got %q", hint)
	}
}

func TestMountPolicyHintNilRegistry(t *testing.T) {
	var r *Registry
	if hint := r.MountPolicyHint(); hint != "" {
		t.Fatalf("nil registry must not panic or print, got %q", hint)
	}
}

// Explicit admin_only is a DECLARATION, not an omission — it must not be
// counted as "never decided", otherwise an operator who carefully marked every
// privileged MCP would still see the warning forever.
func TestMountPolicyHintCountsOnlyOmissions(t *testing.T) {
	r := &Registry{MCPs: map[string]*MCPDef{
		"x": {ID: "x", MountPolicy: MountPolicyAdminOnly},
	}}
	if hint := r.MountPolicyHint(); hint != "" {
		t.Fatalf("explicit admin_only is configured, not unset; got %q", hint)
	}
}

// Wiring test: MountPolicyHint being correct is not enough — it has to be
// PRINTED once by NewRegistry, or the operator never learns the rule. (Same
// class as the round that was lost to a criterion that existed but was never
// called: §28.6 / 铁律 31.)
func TestNewRegistryPrintsMountPolicyHintOnce(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config_test.json")

	r, err := NewRegistry(cfgPath) // no file yet ⇒ empty registry, no hint
	if err != nil {
		t.Fatalf("NewRegistry(empty): %v", err)
	}
	r.Mu.Lock()
	r.MCPs["needs_policy"] = &MCPDef{ID: "needs_policy", URL: "https://a.example.com"}
	r.MCPs["declared_open"] = &MCPDef{ID: "declared_open", URL: "https://b.example.com", MountPolicy: MountPolicyOpen}
	r.Mu.Unlock()
	if err := r.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	if _, err := NewRegistry(cfgPath); err != nil {
		t.Fatalf("NewRegistry(reload): %v", err)
	}
	out := buf.String()
	if got := strings.Count(out, "[mount-policy]"); got != 1 {
		t.Fatalf("hint must be printed exactly once per load, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "1/2") {
		t.Fatalf("hint must report 1 of 2 as unset, got:\n%s", out)
	}
}
