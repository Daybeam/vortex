package schemas

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// G2/§2 (2026-10-24): human-approval bookkeeping for mount-policy `approval`
// MCPs and skills. Extracted from task.go so that file stays under its frozen
// size ratchet.

// ApprovalRecord is the structured audit entry for a human approval. Replaces
// the former map[string]bool so the audit carries the time + scope (and, once
// the resolver is threaded through the choice switch, the approver + channel).
// Backward-compatible with old data: a bare `true` unmarshals to {Approved:true}.
type ApprovalRecord struct {
	Approved   bool      `json:"approved"`
	Time       time.Time `json:"time,omitempty"`
	ApproverID string    `json:"approver_id,omitempty"` // populated when the resolver is threaded (follow-up)
	Channel    string    `json:"channel,omitempty"`     // "web" | "telegram" | "mcp" | "admin"
	Scope      string    `json:"scope,omitempty"`       // "mount" | "skill"
}

// UnmarshalJSON accepts both the legacy bare-bool form (`true`) and the new
// structured object, so existing persisted graphs keep working.
func (r *ApprovalRecord) UnmarshalJSON(data []byte) error {
	if b, err := strconv.ParseBool(strings.TrimSpace(string(data))); err == nil {
		r.Approved = b
		return nil
	}
	type alias ApprovalRecord
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*r = ApprovalRecord(a)
	return nil
}

// ApproveMCPMounts records human approval for the given MCP ids (G2). Called
// only from the SubmitDecision "approve" path — never from agent code.
func (g *TaskGraph) ApproveMCPMounts(ids []string, approverID, channel string) {
	if g == nil || len(ids) == 0 {
		return
	}
	if g.ApprovedMCPMounts == nil {
		g.ApprovedMCPMounts = make(map[string]*ApprovalRecord, len(ids))
	}
	now := time.Now()
	for _, id := range ids {
		if id != "" {
			g.ApprovedMCPMounts[id] = &ApprovalRecord{Approved: true, Time: now, Scope: "mount", ApproverID: approverID, Channel: channel}
		}
	}
}

// IsMCPMountApproved reports whether a human approved mounting mcpID (G2).
func (g *TaskGraph) IsMCPMountApproved(mcpID string) bool {
	if g == nil || mcpID == "" {
		return false
	}
	r := g.ApprovedMCPMounts[mcpID]
	return r != nil && r.Approved
}

// ApproveSkillMounts records human approval for the given skill ids (D3,
// 2026-10-22 — skill granting mirrors the MCP mount gate).
func (g *TaskGraph) ApproveSkillMounts(ids []string, approverID, channel string) {
	if g == nil || len(ids) == 0 {
		return
	}
	if g.ApprovedSkillMounts == nil {
		g.ApprovedSkillMounts = make(map[string]*ApprovalRecord, len(ids))
	}
	now := time.Now()
	for _, id := range ids {
		if id != "" {
			g.ApprovedSkillMounts[id] = &ApprovalRecord{Approved: true, Time: now, Scope: "skill", ApproverID: approverID, Channel: channel}
		}
	}
}

// IsSkillMountApproved reports whether a human approved granting skillID (D3).
func (g *TaskGraph) IsSkillMountApproved(skillID string) bool {
	if g == nil || skillID == "" {
		return false
	}
	r := g.ApprovedSkillMounts[skillID]
	return r != nil && r.Approved
}
