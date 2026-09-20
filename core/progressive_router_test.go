package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestToolRouter_ProgressiveDisclosure(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"coding": {
				ID:             "coding",
				AvailableTools: []string{"read_file", "write_file", "list_files", "apply_patch", "grep_code"},
				Domain:         config.DomainCoding,
			},
			"research": {
				ID:             "research",
				AvailableTools: []string{"google_search", "exa_search", "summarize_url", "extract_pdf"},
				Domain:         config.DomainResearch,
			},
		},
	}

	router := NewToolRouter(reg, nil)

	t.Run("Turn0_GeneralTask_DiscoveryOnly", func(t *testing.T) {
		req := RouteRequest{
			Task: "Help me explore this project and find some info.",
			Bindings: []config.MCPBinding{
				{MCPID: "coding"},
				{MCPID: "research"},
			},
			Turn:            0,
			ProgressiveMode: true,
		}
		routed := router.Route(req)

		// Count tools
		totalVisible := 0
		hasWrite := false
		hasApply := false
		hasList := false
		hasSearch := false

		for _, b := range routed {
			totalVisible += len(b.AllowedTools)
			for _, tool := range b.AllowedTools {
				if tool == "write_file" {
					hasWrite = true
				}
				if tool == "apply_patch" {
					hasApply = true
				}
				if tool == "list_files" {
					hasList = true
				}
				if tool == "google_search" {
					hasSearch = true
				}
			}
		}

		if hasWrite || hasApply {
			t.Errorf("Turn 0 general task should NOT see specialized execution tools")
		}
		if !hasList || !hasSearch {
			t.Errorf("Turn 0 general task SHOULD see discovery tools")
		}
		t.Logf("Total visible tools in Turn 0: %d", totalVisible)
	})

	t.Run("Turn0_SpecificTask_AllTools", func(t *testing.T) {
		req := RouteRequest{
			Task: "Fix the bug in main.go and apply the patch.",
			Bindings: []config.MCPBinding{
				{MCPID: "coding"},
			},
			Turn:            0,
			ProgressiveMode: true,
		}
		routed := router.Route(req)

		hasApply := false
		for _, b := range routed {
			for _, tool := range b.AllowedTools {
				if tool == "apply_patch" {
					hasApply = true
				}
			}
		}

		if !hasApply {
			t.Errorf("Turn 0 specific task SHOULD see specialized tools mentioned in task")
		}
	})

	t.Run("Turn1_GeneralTask_Expanded", func(t *testing.T) {
		req := RouteRequest{
			Task: "Help me explore this project. Assistant: I found main.go, I should edit it.",
			Bindings: []config.MCPBinding{
				{MCPID: "coding"},
			},
			Turn:            1,
			ProgressiveMode: true,
		}
		routed := router.Route(req)

		hasWrite := false
		for _, b := range routed {
			for _, tool := range b.AllowedTools {
				if tool == "write_file" {
					hasWrite = true
				}
			}
		}

		if !hasWrite {
			t.Errorf("Turn 1 task with specific intent SHOULD see expanded tool set")
		}
	})
}
