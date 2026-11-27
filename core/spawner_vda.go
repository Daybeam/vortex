package core

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

type screenshotSender interface {
	SendRequest(context.Context, string, any) (any, error)
}

// vdaState holds the per-turn accumulators the forced visual audit appends to.
type vdaState struct {
	turnAttachments []schemas.Attachment
	captured        []schemas.Attachment
	toolResults     []string
}

// runForcedVisualAudit asks the first bound MCP that exposes capture_screenshot.
// Every attempted capture emits a terminal status; unavailable and failed
// captures are observations too, never evidence that an image was attached.
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
		if !containsTool(mcp.AvailableTools, "capture_screenshot") {
			continue
		}
		cli, err := s.mcpMgr.ensureMCPClient(ctx, mcp, req.TaskID)
		if err != nil {
			s.recordVDACapture(req, "client_error", err.Error(), nil)
			st.toolResults = append(st.toolResults, "System: Automatic verification screenshot could not be captured: "+err.Error())
			return
		}
		s.captureForcedVisualAudit(ctx, cli, req, b.MCPID, st)
		return
	}
	s.recordVDACapture(req, "unavailable", "no bound MCP exposes capture_screenshot", nil)
	st.toolResults = append(st.toolResults, "System: Automatic verification screenshot unavailable: no bound MCP exposes capture_screenshot.")
}

func (s *Spawner) captureForcedVisualAudit(ctx context.Context, cli screenshotSender, req *SpawnRequest, mcpID string, st *vdaState) {
	screen, err := cli.SendRequest(ctx, "tools/call", map[string]any{
		"name":      "capture_screenshot",
		"arguments": map[string]any{},
	})
	if err != nil {
		s.recordVDACapture(req, "capture_error", err.Error(), nil)
		st.toolResults = append(st.toolResults, "System: Automatic verification screenshot could not be captured: "+err.Error())
		return
	}
	out, ok := screen.(schemas.SubagentOutput)
	if !ok {
		s.recordVDACapture(req, "invalid_response", fmt.Sprintf("unexpected response type %T", screen), nil)
		st.toolResults = append(st.toolResults, "System: Automatic verification screenshot could not be captured: invalid MCP response.")
		return
	}
	refs := make([]map[string]string, 0, len(out.Attachments))
	for _, att := range out.Attachments {
		ref := map[string]string{"mime_type": att.MimeType, "path": att.Path}
		if data, err := os.ReadFile(att.Path); err == nil {
			ref["sha256"] = fmt.Sprintf("%x", sha256.Sum256(data))
		} else {
			ref["sha256"] = "unreadable"
		}
		refs = append(refs, ref)
	}
	if len(out.Attachments) == 0 {
		s.recordVDACapture(req, "empty", "capture_screenshot returned no attachments", refs)
		st.toolResults = append(st.toolResults, "System: Automatic verification screenshot could not be captured: no attachment returned.")
		return
	}
	st.turnAttachments = append(st.turnAttachments, out.Attachments...)
	st.captured = append(st.captured, out.Attachments...)
	s.recordVDACapture(req, "captured", "", refs)
	st.toolResults = append(st.toolResults, "System: Automatic verification screenshot captured.")
}

func (s *Spawner) recordVDACapture(req *SpawnRequest, status, reason string, refs []map[string]string) {
	s.logger.Log(obs.EventVDACapture, req.TaskID, req.StepID, map[string]any{
		"capture_status": status,
		"reason":         reason,
		"attachments":    refs,
	})
}

func appendUniqueAttachments(base, extra []schemas.Attachment) []schemas.Attachment {
	seen := make(map[string]struct{}, len(base)+len(extra))
	for _, att := range base {
		seen[att.MimeType+"\x00"+att.Path] = struct{}{}
	}
	for _, att := range extra {
		key := att.MimeType + "\x00" + att.Path
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		base = append(base, att)
	}
	return base
}
