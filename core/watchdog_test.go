package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestGatewayWatchdog_Sniff(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"google": {
				ID:             "google",
				AvailableTools: []string{"google_search"},
				Provides:       []string{"web_search"},
				Source:         config.SourceExternalDynamic,
			},
			"exa": {
				ID:             "exa",
				AvailableTools: []string{"exa_search"},
				Provides:       []string{"web_search"},
				Source:         config.SourceExternalDynamic,
			},
			"local": {
				ID:             "local",
				AvailableTools: []string{"read_file"},
				Provides:       []string{"filesystem"},
				Source:         config.SourceInternalStatic,
			},
		},
	}

	watchdog := NewGatewayWatchdog(reg)
	hub := &ContextHub{Registry: reg}

	t.Run("NoConflict_SingleTool", func(t *testing.T) {
		calls := []schemas.ToolCall{
			{Name: "google_search"},
		}
		alert := watchdog.Sniff(calls, hub)
		if alert != nil {
			t.Errorf("expected no alert, got %v", alert)
		}
	})

	t.Run("NoConflict_MixedSources", func(t *testing.T) {
		calls := []schemas.ToolCall{
			{Name: "google_search"},
			{Name: "read_file"},
		}
		alert := watchdog.Sniff(calls, hub)
		if alert != nil {
			t.Errorf("expected no alert, got %v", alert)
		}
	})

	t.Run("Conflict_OverlappingCapabilities", func(t *testing.T) {
		calls := []schemas.ToolCall{
			{Name: "google_search"},
			{Name: "exa_search"},
		}
		alert := watchdog.Sniff(calls, hub)
		if alert == nil {
			t.Fatal("expected watchdog alert")
		}
		if alert.Type != "multi_source_conflict_probing" {
			t.Errorf("expected type multi_source_conflict_probing, got %s", alert.Type)
		}
		t.Logf("Alert Message: %s", alert.Message)
	})

	t.Run("Conflict_WithSkills", func(t *testing.T) {
		reg.Skills = map[string]*config.Skill{
			"bing": {
				ID:         "bing",
				Name:       "bing_search",
				Capability: "web_search",
				Source:     config.SourceExternalDynamic,
			},
		}
		calls := []schemas.ToolCall{
			{Name: "google_search"},
			{Name: "bing"},
		}
		alert := watchdog.Sniff(calls, hub)
		if alert == nil {
			t.Fatal("expected watchdog alert")
		}
		t.Logf("Alert Message (Skill + MCP): %s", alert.Message)
	})
}
