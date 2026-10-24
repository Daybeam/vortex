package core

import (
	"context"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// vdaState holds the per-turn accumulators the forced visual audit appends to.
type vdaState struct {
	turnAttachments []schemas.Attachment
	toolResults     []string
}

// runForcedVisualAudit implements the VDA (Forced Visual Audit): when a turn
// needs verification, ask the first bound MCP that exposes `capture_screenshot`
// for one, appending the attachment (and a marker line for the model).
//
// Extracted from doSpawn (size gate); behaviour is identical, including the
// `break` after the FIRST MCP that exposes the tool (attempted or not) and the
// silence on every error path.
func (s *Spawner) runForcedVisualAudit(
	ctx context.Context, req *SpawnRequest, bindings []config.MCPBinding,
	needsVerification bool, st *vdaState,
) {
	if !needsVerification {
		return
	}
	for _, b := range bindings {
		mcp := req.Hub.GetMCP(b.MCPID)
		if mcp == nil {
			continue
		}
		if containsTool(mcp.AvailableTools, "capture_screenshot") {
			cli, err := s.mcpMgr.ensureMCPClient(ctx, mcp, req.TaskID)
			if err == nil {
				screen, err := cli.SendRequest(ctx, "tools/call", map[string]any{
					"name":      "capture_screenshot",
					"arguments": map[string]any{},
				})
				if err == nil {
					if att, ok := screen.(schemas.SubagentOutput); ok {
						st.turnAttachments = append(st.turnAttachments, att.Attachments...)
					}
					st.toolResults = append(st.toolResults, "System: Automatic verification screenshot captured.")
				}
			}
			break
		}
	}
}
