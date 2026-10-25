package core

import (
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// coreToolPolicy answers one question: may this role call built-in _core tools,
// and which ones?
//
// Why this exists (2026-10-19): buildMCPServers attaches the _core server to
// every spawn, so every role — including task_planner — gets write_file,
// read_file and execute_code. A planner that stops planning and starts doing
// the task is not a hypothetical: it already has the tools and a sandbox to run
// them in. Restricting the schema is only half a defence, because
// spawner_tool_exec.go dispatches on CoreToolNames[call.Name] with no role
// check — a model that names execute_code gets it executed whether or not it
// was advertised. So the same policy is applied at both ends.
type coreToolPolicy struct {
	restricted bool            // false = every core tool is allowed
	allowed    map[string]bool // meaningful only when restricted
}

// allows reports whether name (a core tool) may be dispatched for this role.
func (p coreToolPolicy) allows(name string) bool {
	if !p.restricted {
		return true
	}
	return p.allowed[name]
}

// filter drops core tool definitions the role may not use. It must preserve
// order and the input slice's identity semantics (returns a new slice only when
// something is actually removed).
func (p coreToolPolicy) filter(defs []schemas.ToolDefinition) []schemas.ToolDefinition {
	if !p.restricted {
		return defs
	}
	out := make([]schemas.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		if p.allowed[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// coreToolPolicyFor resolves the policy for roleID.
//
// Semantics of Role.CoreTools (A6 assessed 2026-10-22; default-on REINSTATED
// by product decision the same day — see coreToolPolicyFromRole):
//   - nil (field absent)  -> unrestricted (the default; core tools are part
//     of the engine's operating mechanism)
//   - non-nil, length 0   -> no core tools at all (task_planner's case)
//   - non-nil, length > 0 -> exactly those names
//   - contains "*"        -> unrestricted (explicit, self-documenting form)
//
// Names outside CoreToolNames are ignored rather than trusted: a typo in config
// must not be able to widen the surface.
func (s *Spawner) coreToolPolicyFor(roleID string) coreToolPolicy {
	if s == nil {
		return coreToolPolicy{} // unrestricted: nil-safety over strictness
	}
	return coreToolPolicyForRole(s.registry, roleID)
}

// coreToolPolicyForRole is the Spawner-independent form used where only a
// *config.Registry is at hand (planner, capability reporting).
func coreToolPolicyForRole(reg *config.Registry, roleID string) coreToolPolicy {
	if reg == nil || roleID == "" {
		return coreToolPolicy{}
	}
	reg.Mu.RLock()
	var r *config.Role
	if found, ok := reg.Roles[roleID]; ok {
		r = found
	}
	reg.Mu.RUnlock()
	return coreToolPolicyFromRole(r)
}

// @decision:DECISION-CORETOOLS-DEFAULT
// coreToolPolicyFromRole is the form used where the *config.Role is already in
// hand but its ID is not — PromptAssembler.Build receives the resolved role,
// not the roleID. Both forms must agree, because the prompt and the dispatcher
// are two ends of the same promise: a surface advertised in the system prompt
// but refused at dispatch (or vice versa) is a lie either way.
func coreToolPolicyFromRole(r *config.Role) coreToolPolicy {
	if r == nil || r.CoreTools == nil {
		// A6 (eval FINDINGS.md §A6) assessed 2026-10-22: the same-day flip to
		// "absent = none" was REVERSED by product decision — core tools are
		// part of the engine's operating mechanism, not an optional extra:
		// step hand-offs flow through the shared output directory
		// (write_file/read_file, see prompt_assembler's SHARED-output
		// instruction), and verification methods name read_file. A role that
		// says nothing keeps the default surface; narrowing stays opt-in per
		// role (task_planner declares []).
		return coreToolPolicy{}
	}
	allowed := make(map[string]bool, len(r.CoreTools))
	for _, n := range r.CoreTools {
		if n == "*" {
			// Explicit "all of them" — same result as nil, self-documenting.
			return coreToolPolicy{}
		}
		if CoreToolNames[n] {
			allowed[n] = true
		}
	}
	return coreToolPolicy{restricted: true, allowed: allowed}
}
