package chat

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// Extracted from core/antipattern_injection_test.go during the pkg/chat
// extraction. These tests need white-box access to ChatHarness (buildSystem)
// so they live in package chat.

func TestChatHarnessSystemPrompt_ContainsReadinessGate(t *testing.T) {
	h := &ChatHarness{}
	system := h.buildSystem()
	if !strings.Contains(system, "DELEGATION READINESS GATE") {
		t.Error("system prompt must contain the Readiness Gate header")
	}
	if !strings.Contains(system, "NEVER blindly delegate a vague task") {
		t.Error("system prompt must contain the hard-rule phrasing")
	}
}

func TestChatHarnessDelegateTool_HasReadinessSelfCheck(t *testing.T) {
	h := &ChatHarness{}
	tools := h.ToolDefinitions()
	var delegate *schemas.ToolDefinition
	for i := range tools {
		if tools[i].Name == "delegate_to_orchestrator" {
			delegate = &tools[i]
			break
		}
	}
	if delegate == nil {
		t.Fatal("delegate_to_orchestrator tool not found")
	}
	if !strings.Contains(delegate.Description, "READINESS GATE") {
		t.Error("delegate tool description must mention READINESS GATE")
	}
	props, ok := delegate.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("delegate tool schema missing properties")
	}
	if _, ok := props["readiness_self_check"]; !ok {
		t.Error("delegate tool schema must include readiness_self_check field")
	}
	promptSpec, _ := props["prompt"].(map[string]any)
	desc, _ := promptSpec["description"].(string)
	if !strings.Contains(desc, "MUST include") {
		t.Error("prompt description must specify the required components for readiness")
	}
}
