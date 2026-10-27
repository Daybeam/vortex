package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Mount-policy values, checked by ResolveFinalMCPs (MCPs) and
// ResolveFinalSkills (skills). See
// docs/completed/2026-10-22/MCP_MOUNT_POLICY_AND_TIER_PROPAGATION_DESIGN.md (MCPs) and
// docs/plan/MOUNT_POLICY_REMAINDER_DESIGN.md §2.R-b (skills, D3).
const (
	// MountPolicyOpen: any submitter tier may mount/use the resource.
	MountPolicyOpen = "open"
	// MountPolicyApproval: non-admin use requires a human_approval_required
	// decision (approve records it on the graph). Admin uses without approval.
	MountPolicyApproval = "approval"
	// MountPolicyAdminOnly: requires admin tier.
	MountPolicyAdminOnly = "admin_only"
)

// effectiveMountPolicy resolves a raw policy string against a fallback for
// empty/unknown values. MCPs fall back to admin_only (safe default); skills
// fall back to open (prompt-only surface — no executable capability).
func effectiveMountPolicy(policy, fallback string) string {
	switch policy {
	case MountPolicyOpen, MountPolicyApproval, MountPolicyAdminOnly:
		return policy
	default:
		return fallback
	}
}

// EffectiveMountPolicy returns the policy to apply for an MCP mount (bound or
// dynamic), defaulting to MountPolicyAdminOnly for empty/unknown values.
func (m *MCPDef) EffectiveMountPolicy() string {
	if m == nil {
		return MountPolicyAdminOnly
	}
	return effectiveMountPolicy(m.MountPolicy, MountPolicyAdminOnly)
}

// EffectiveMountPolicy returns the policy to apply when granting this skill
// (bound or dynamic). Skills default to open — their implementations are
// prompt-level (SkillImplementation.SystemPrompt) with no executable surface;
// real executables (MCPs, core tools) are gated separately. Mark a skill
// approval/admin_only to gate sensitive prompt content (e.g. credentials
// guidance) for non-admin submitters.
func (s *Skill) EffectiveMountPolicy() string {
	if s == nil {
		return MountPolicyOpen
	}
	return effectiveMountPolicy(s.MountPolicy, MountPolicyOpen)
}

// ServerRiskyMCP reports whether mounting this MCP could put the SERVER (not
// just the caller's own data) at risk, and therefore must stay admin-managed
// even when the caller supplied the definition themselves.
//
// This is the dividing line for the multi-tenant rule (2026-10-26):
//
//	tenant-owned + not server-risky  → the tenant may use it without an admin
//	tenant-owned + server-risky      → admin_only, always
//	operator-owned + no policy       → admin_only (unchanged default)
//
// Two ways an MCP definition reaches the HOST:
//
//  1. It launches a local process (`command`). Unless the operator marked it
//     `sandboxed`, that process runs with the engine's own privileges — file
//     system, credentials, network of the whole machine.
//  2. It points at a URL on the loopback/private range. That is not "a remote
//     API"; it is an HTTP client aimed at the host itself or its LAN
//     (SSRF): http://127.0.0.1:9200, http://169.254.169.254, http://10.0.0.5.
//
// And one way it reaches the operator's IDENTITY (third axis, 2026-10-26):
//
//  3. It carries an operator credential (APIKeyEnv / URLAuthPlaceholder). The
//     remote service then sees the operator, so any caller can read and write
//     the operator's data there. See CarriesOperatorCredential and
//     docs/plan/OAUTH_SHARED_MCP_MULTI_TENANT_DESIGN.md.
//
// Everything else (a public URL with no credential) is tenant-scoped risk: the
// tenant can only reach what they already could reach with a curl, so gating it
// behind an operator decision would make a public tenant unable to complete a
// closed loop — the exact problem this rule exists to solve.
//
// NOTE on axis 3 vs an explicit policy: this function answers "would an
// UNDECLARED mount be dangerous". An operator who writes mount_policy=open on a
// credential-carrying MCP has declared the risk acceptable, and the bound/named
// paths honour that (see resolveMountPolicyGate). What it must never do is let
// such an MCP be treated as an ordinary public API by DEFAULT.
//
// Deliberately conservative: unknown/unparseable definitions are risky.
func ServerRiskyMCP(m *MCPDef) bool {
	if m == nil {
		return true
	}
	// Read the PLATFORM-RESOLVED command, not m.Command. A definition with
	// `command: ""` plus a `platform_overrides.<goos>.command` launches a real
	// local process on that OS, so inspecting the base field alone is a
	// one-field bypass (config.MCPDef.ResolveForPlatform is what the spawner
	// actually executes).
	cmd, _, _, _ := m.ResolveForPlatform()
	if strings.TrimSpace(cmd) != "" && !m.Sandboxed {
		return true
	}
	if u := strings.TrimSpace(m.URL); u != "" {
		if host := urlHost(u); host != "" && isInternalHost(host) {
			return true
		}
	}
	// Axis 3: not "can it reach the host" but "whose identity does the remote
	// see". A public URL carrying the operator's token is not tenant-scoped
	// risk — it is impersonation with the operator's account.
	if m.CarriesOperatorCredential() {
		return true
	}
	return false
}

// CarriesOperatorCredential reports whether this definition authenticates to the
// remote side as the OPERATOR rather than as the caller.
//
// The test is APIKeyEnv / URLAuthPlaceholder being set, and it is exact rather
// than heuristic: both fields name a PROCESS ENVIRONMENT variable
// (`cachedGetenv` in core/mcp_manager.go), and an env var can only be set by
// whoever started the engine. A public caller cannot supply their own value,
// so the resulting credential is always the operator's identity.
//
// Why it belongs with ServerRiskyMCP: reaching the host is not the only way to
// put the server at risk. Presenting the operator's identity to a remote service
// lets a caller read and write data as the operator, and the engine cannot see
// that leak at all — core/egress_redaction.go only scans text that came back,
// and per-user data does not arrive labelled "owner: …".
//
// When MCPDef.CredentialScope is implemented (design §4 L1), this becomes
// `… && EffectiveCredentialScope() == CredentialOperator`; until then the
// default IS operator, because that is what the code does today.
func (m *MCPDef) CarriesOperatorCredential() bool {
	if m == nil {
		return false
	}
	return strings.TrimSpace(m.APIKeyEnv) != "" || strings.TrimSpace(m.URLAuthPlaceholder) != ""
}

// urlHost extracts the hostname from a URL-ish string, tolerating "host:port"
// without a scheme (MCP configs use both). Returns "" when nothing usable is
// there, which ServerRiskyMCP treats as not-risky — a typo'd URL will fail to
// connect anyway, and refusing it here would break configs before they load.
func urlHost(raw string) string {
	s := raw
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	h := u.Hostname()
	if h == "" {
		// Unparseable authority (e.g. a template like ${API_HOST}) — leave the
		// decision to the connect step.
		return ""
	}
	return h
}

// isInternalHost covers loopback, RFC1918, link-local (incl. the cloud metadata
// address 169.254.169.254), IPv6 ULA/loopback, and the usual "localhost"
// spellings. Hostnames that merely RESOLVE to a private IP are not caught here
// — that needs a DNS lookup at mount time, which is a separate (and much more
// expensive) check; a tenant can already point curl at such a host.
func isInternalHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	// Strip a bracketed IPv6 form: [::1] → ::1
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	switch h {
	case "localhost", "localhost.localdomain", "ip6-localhost", "ip6-loopback":
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified()
	}
	return false
}

// MountPolicyHint returns a ONE-LINE summary of how many MCPs in the registry
// have no explicit mount_policy, or "" when everything is declared.
//
// Per operator decision (2026-10-26): do NOT dump the list — with 20+ MCPs
// every boot would print 20 lines nobody reads. One line that states the RULE
// is enough, because the rule ("no explicit policy ⇒ not exposed to public")
// is what an operator needs to know; the per-MCP answer is one `grep
// mount_policy` away.
func (r *Registry) MountPolicyHint() string {
	if r == nil {
		return ""
	}
	r.Mu.RLock()
	defer r.Mu.RUnlock()
	total, unset := 0, 0
	for _, m := range r.MCPs {
		if m == nil {
			continue
		}
		total++
		if effectiveMountPolicy(m.MountPolicy, MountPolicyAdminOnly) == MountPolicyAdminOnly &&
			strings.TrimSpace(m.MountPolicy) == "" {
			unset++
		}
	}
	if unset == 0 {
		return ""
	}
	return fmt.Sprintf("[mount-policy] %d/%d MCP(s) declare no mount_policy ⇒ admin_only, "+
		"NOT exposed to the public tier (set \"mount_policy\":\"open\" to publish one; "+
		"public tenants can still mount their own non-local MCPs via additional_mcps)",
		unset, total)
}
