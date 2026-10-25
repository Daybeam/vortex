package core

import (
	"fmt"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Approval-authority enforcement (Stage 1, 2026-10-24).
// See docs/plan/DECISION_APPROVAL_AUTHORITY_DESIGN.md §2/§7.
//
// This is the engine-side check that "who can approve a human_approval_required
// decision" is enforced regardless of entry point (HTTP admin, public web,
// Telegram, MCP tool) — none of those go through the tools/ tier check, so the
// gate lives here, on SubmitDecisionWithResolver.

// Resolver is the identity of whoever is resolving a decision (the approver).
type Resolver struct {
	OwnerID string
	IsAdmin bool
	IsOwner bool   // public web endpoint: treat as the task's owner (graph.OwnerID)
	Channel string // "web" | "telegram" | "mcp" | "admin"
}

// SubmitDecisionWithResolver resolves a decision after validating the resolver's
// authority (approver level + expiry) for human_approval_required decisions.
// Other decision types are unaffected. Callers without a resolver continue to
// use SubmitDecision / SubmitDecisionWithPayload (which resolve as admin).
func (s *DirectedEngine) SubmitDecisionWithResolver(taskID, decisionID, choice, payload string, resolver Resolver) error {
	if err := s.checkApprovalAuthority(taskID, decisionID, resolver); err != nil {
		return err
	}
	// Stash the resolver on the decision's context so applyApproveChoice can
	// record the approver + channel in the ApprovalRecord (§2, 2026-10-24).
	s.Mu.Lock()
	if graph := s.graphs[taskID]; graph != nil {
		for _, d := range graph.PendingDecisions {
			if d.ID == decisionID {
				if d.Context == nil {
					d.Context = map[string]any{}
				}
				d.Context["_resolver_owner_id"] = resolver.OwnerID
				d.Context["_resolver_channel"] = resolver.Channel
				break
			}
		}
	}
	s.Mu.Unlock()
	return s.SubmitDecisionWithPayload(taskID, decisionID, choice, payload)
}

// checkApprovalAuthority enforces, for a human_approval_required decision:
//   - expiry: if ApprovalExpiry > 0 and the decision is older than that, reject;
//   - approver level: if any named MCP requires admin (Approver != "owner"), the
//     resolver must be admin; otherwise (all owner-level) the task owner or an
//     admin may approve (self-approve allowed — §7 decision 1).
//
// Non-human-approval decisions and missing graphs/decisions pass through (the
// latter so SubmitDecisionWithPayload can return its own "not found" error).
// @decision:DECISION-APPROVAL-AUTHORITY
func (s *DirectedEngine) checkApprovalAuthority(taskID, decisionID string, resolver Resolver) error {
	// Hold RLock through the entire function: graph.PendingDecisions and
	// graph.OwnerID are mutated under s.Mu.Lock() elsewhere (decision gate,
	// status, delegation fixes). Reading them without the lock is a data race
	// on the slice header / fields.
	// (audit CS-1: data race on graph.PendingDecisions, 2026-10-24)
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	graph := s.graphs[taskID]
	if graph == nil {
		return nil
	}
	var dec *schemas.Decision
	for _, d := range graph.PendingDecisions {
		if d.ID == decisionID {
			dec = d
			break
		}
	}
	if dec == nil || dec.Type != schemas.DecisionHumanApprovalRequired {
		return nil
	}
	if exp := s.approvalExpiry(); exp > 0 && !dec.Created.IsZero() && time.Since(dec.Created) > exp {
		return fmt.Errorf("approval decision %q has expired (limit %s); re-trigger the task to regenerate it", dec.ID, exp)
	}
	requireAdmin := false
	for _, mcpID := range decisionApprovalMounts(dec) {
		if mcp := s.registry.GetMCP(mcpID); mcp == nil || mcp.Approver != "owner" {
			requireAdmin = true
			break
		}
	}
	if requireAdmin {
		if !resolver.IsAdmin {
			return fmt.Errorf("decision %q requires an admin approver", dec.ID)
		}
	} else if !resolver.IsAdmin && !resolver.IsOwner && resolver.OwnerID != graph.OwnerID {
		return fmt.Errorf("decision %q requires the task owner or an admin approver", dec.ID)
	}
	return nil
}

// approvalExpiry returns the human-approval expiry duration. nil/missing ⇒
// 30m default; *0 or negative ⇒ 0 (never expires — §7 decision 3).
func (s *DirectedEngine) approvalExpiry() time.Duration {
	if s.registry == nil {
		return 30 * time.Minute
	}
	e := s.registry.System.ApprovalExpiry
	if e == nil {
		return 30 * time.Minute
	}
	if *e <= 0 {
		return 0
	}
	return time.Duration(*e) * time.Second
}
