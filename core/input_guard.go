package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// GuardViolationError represents a structured capability-guided error
// designed for self-correcting agents.
type GuardViolationError struct {
	Message          string   `json:"message"`
	ViolationType    string   `json:"violation_type"`
	CurrentLength    int      `json:"current_length"`
	AllowedThreshold int      `json:"allowed_threshold"`
	AvailableRoles   []string `json:"available_roles"`
	AvailableMCPs    []string `json:"available_mcps"`
	CorrectiveAction string   `json:"corrective_action"`
}

func (e *GuardViolationError) Error() string {
	return fmt.Sprintf(
		"[GUARD ERROR - %s]: %s\n"+
			"📊 Metric: Input size %d chars exceeds threshold %d chars.\n"+
			"👉 DYNAMIC CAPABILITY REFERENCE:\n"+
			"- Available Roles: [%s]\n"+
			"- Available MCPs: [%s]\n"+
			"💡 CORRECTIVE ACTION:\n%s",
		e.ViolationType, e.Message, e.CurrentLength, e.AllowedThreshold,
		strings.Join(e.AvailableRoles, ", "),
		strings.Join(e.AvailableMCPs, ", "),
		e.CorrectiveAction,
	)
}

// EvaluateAndGuardInputs inspects incoming task inputs before graph construction.
// It enforces hard constraints on payload size and guides the agent with live capabilities.
func EvaluateAndGuardInputs(inputs []schemas.StepInput, registry *config.Registry) error {
	maxSafeTaskChars := getMaxSafeTaskChars(registry)

	for _, inp := range inputs {
		// 1. Check if raw text payload is excessively large
		if len(inp.Task) > maxSafeTaskChars && !strings.HasPrefix(inp.Task, "asset://") {

			// 2. Query live environment capabilities dynamically from the registry
			var activeRoles []string
			if registry != nil && registry.Roles != nil {
				for roleID := range registry.Roles {
					activeRoles = append(activeRoles, roleID)
				}
			} else {
				activeRoles = []string{"book_researcher", "analyst", "coder"} // fallback
			}

			var activeMCPs []string
			if registry != nil && registry.MCPs != nil {
				for mcpID := range registry.MCPs {
					activeMCPs = append(activeMCPs, mcpID)
				}
			} else {
				activeMCPs = []string{"leann-mcp", "document-parser"} // fallback
			}

			// 3. Construct structured actionable hint
			correctivePlaybook :=
				"1. Do not ingest raw long text directly into task instructions.\n" +
					"2. Side-load the document into StagedWorkspace or use an appropriate MCP (e.g., leann-mcp).\n" +
					"3. Delegate the subtask to a specialized worker role using additional_mcps.\n" +
					"Example Payload:\n" +
					"{\n" +
					"  \"role_id\": \"" + selectBestRole(activeRoles) + "\",\n" +
					"  \"additional_mcps\": [\"" + selectBestMCP(activeMCPs) + "\"],\n" +
					"  \"task\": \"Analyze document referenced via asset://...\"\n" +
					"}"

			return &GuardViolationError{
				Message:          "Direct raw text overload detected. Main Agent context preservation violated.",
				ViolationType:    "PAYLOAD_SIZE_EXCEEDED",
				CurrentLength:    len(inp.Task),
				AllowedThreshold: maxSafeTaskChars,
				AvailableRoles:   activeRoles,
				AvailableMCPs:    activeMCPs,
				CorrectiveAction: correctivePlaybook,
			}
		}
	}
	return nil
}

func selectBestRole(roles []string) string {
	for _, r := range roles {
		if strings.Contains(r, "researcher") || strings.Contains(r, "analyst") {
			return r
		}
	}
	if len(roles) > 0 {
		return roles[0]
	}
	return "worker"
}

func selectBestMCP(mcps []string) string {
	for _, m := range mcps {
		if strings.Contains(m, "leann") || strings.Contains(m, "doc") {
			return m
		}
	}
	if len(mcps) > 0 {
		return mcps[0]
	}
	return "mcp-server"
}

// getMaxSafeTaskChars returns the model-tier-aware payload size threshold.
// Priority: (1) explicit config override registry.System.MaxTaskChars,
// (2) 50% of the default provider's GetMaxSystemPromptChars(),
// (3) hardcoded fallback 5000.
func getMaxSafeTaskChars(registry *config.Registry) int {
	const fallback = 5000
	if registry == nil {
		return fallback
	}
	// (1) Config override
	if registry.System.MaxTaskChars > 0 {
		return registry.System.MaxTaskChars
	}
	// (2) Model-tier-aware: 50% of system prompt char budget
	if registry.DefaultProvider != "" && registry.Providers != nil {
		if p, ok := registry.Providers[registry.DefaultProvider]; ok && p != nil {
			half := p.GetMaxSystemPromptChars() / 2
			if half > fallback {
				return half
			}
		}
	}
	return fallback
}
