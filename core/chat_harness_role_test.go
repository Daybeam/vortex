package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestChatHarness_BuildSystem_WithRole(t *testing.T) {
	h := &ChatHarness{
		Role: &config.Role{
			Name:        "Market Analyst",
			Instruction: "You are a financial market analyst with deep expertise in equity research.",
		},
	}

	sys := h.buildSystem("test query", "test_task")

	if !strings.Contains(sys, "# Role: Market Analyst") {
		t.Error("expected role header in system prompt")
	}
	if !strings.Contains(sys, "You are a financial market analyst") {
		t.Error("expected role instruction in system prompt")
	}
	if !strings.Contains(sys, "You are a concise, helpful assistant") {
		t.Error("expected base system prompt to still be present")
	}
}

func TestChatHarness_BuildSystem_WithoutRole(t *testing.T) {
	h := &ChatHarness{}

	sys := h.buildSystem("test query", "test_task")

	if strings.Contains(sys, "# Role:") {
		t.Error("expected no role header when Role is nil")
	}
	if !strings.Contains(sys, "You are a concise, helpful assistant") {
		t.Error("expected base system prompt")
	}
}

func TestChatHarness_BuildSystem_EmptyInstruction(t *testing.T) {
	h := &ChatHarness{
		Role: &config.Role{
			Name:        "Empty Role",
			Instruction: "",
		},
	}

	sys := h.buildSystem("test query", "test_task")

	if strings.Contains(sys, "# Role:") {
		t.Error("expected no role header when Instruction is empty")
	}
}
