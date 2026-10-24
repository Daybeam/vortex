package config

// CallableByAdmin reports whether a caller at the given admin tier may invoke
// this role (AUTH-1). An empty CallableTiers means open — any tier — which is
// the pre-existing behavior, so only roles that explicitly opt in are
// restricted. The built-in meta roles (task_planner / decision_arbiter /
// sop_generator) declare ["admin"]; see seed_builtins.go.
//
// Lives here rather than next to the Role struct so config/types.go (already at
// its size-ratchet ceiling) does not grow. Enforced at the engine submit
// chokepoint — core/scheduler_submit_gate.go roleCallableBySubmitter.
func (r *Role) CallableByAdmin(isAdmin bool) bool {
	if r == nil || len(r.CallableTiers) == 0 {
		return true
	}
	for _, t := range r.CallableTiers {
		switch t {
		case "*":
			return true
		case "admin":
			if isAdmin {
				return true
			}
		case "public":
			if !isAdmin {
				return true
			}
		}
	}
	return false
}
