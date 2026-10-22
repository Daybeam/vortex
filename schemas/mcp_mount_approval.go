package schemas

// G2 (2026-10-22): human-approval bookkeeping for mount-policy `approval` MCPs.
// Extracted from task.go so that file stays under its frozen size ratchet.

// ApproveMCPMounts records human approval for the given MCP ids (G2). Called
// only from the SubmitDecision "approve" path — never from agent code.
func (g *TaskGraph) ApproveMCPMounts(ids []string) {
	if g == nil || len(ids) == 0 {
		return
	}
	if g.ApprovedMCPMounts == nil {
		g.ApprovedMCPMounts = make(map[string]bool, len(ids))
	}
	for _, id := range ids {
		if id != "" {
			g.ApprovedMCPMounts[id] = true
		}
	}
}

// IsMCPMountApproved reports whether a human approved mounting mcpID (G2).
func (g *TaskGraph) IsMCPMountApproved(mcpID string) bool {
	return g != nil && mcpID != "" && g.ApprovedMCPMounts[mcpID]
}

// ApproveSkillMounts records human approval for the given skill ids (D3,
// 2026-10-22 — skill granting mirrors the MCP mount gate). Called only from
// the SubmitDecision "approve" path — never from agent code.
func (g *TaskGraph) ApproveSkillMounts(ids []string) {
	if g == nil || len(ids) == 0 {
		return
	}
	if g.ApprovedSkillMounts == nil {
		g.ApprovedSkillMounts = make(map[string]bool, len(ids))
	}
	for _, id := range ids {
		if id != "" {
			g.ApprovedSkillMounts[id] = true
		}
	}
}

// IsSkillMountApproved reports whether a human approved granting skillID (D3).
func (g *TaskGraph) IsSkillMountApproved(skillID string) bool {
	return g != nil && skillID != "" && g.ApprovedSkillMounts[skillID]
}
