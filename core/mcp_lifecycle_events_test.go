package core

import (
	"testing"
)

// TestMCPLifecycleEvents_AreRouteableByAdmin pins the contract an admin
// NotificationRoute depends on (2026-10-23): the MCP up/down lifecycle events
// must have stable, predictable names on the EventBus. Before this they were
// raw string literals ("EventMCPClientFailed", "EventMCPToolsDiscovered"), so a
// config route could not reasonably trigger on them.
func TestMCPLifecycleEvents_AreRouteableByAdmin(t *testing.T) {
	// 1. The names are the normalized snake_case values a route names.
	want := map[string]EventType{
		"mcp_online":           EventMCPOnline,
		"mcp_offline":          EventMCPOffline,
		"mcp_discovery_failed": EventMCPDiscoveryFailed,
	}
	for name, ev := range want {
		if string(ev) != name {
			t.Errorf("event %q has value %q", name, string(ev))
		}
	}

	// 2. End-to-end: logger.Log publishes to DefaultBus under that exact name,
	//    which is what webhook_notifier.matchTriggerOn compares against.
	logger := mustNewLogger(t, t.TempDir(), nil)

	got := make(chan string, 1)
	unsub := DefaultBus.SubscribeWithCancel("mcp_offline", func(e AgentEvent) error {
		select {
		case got <- e.EventType:
		default:
		}
		return nil
	})
	defer unsub()

	logger.Log(EventMCPOffline, "task-1", "s1", map[string]any{"mcp": "demo"})

	select {
	case name := <-got:
		if name != "mcp_offline" {
			t.Fatalf("subscriber saw %q, want mcp_offline", name)
		}
	default:
		t.Fatal("no mcp_offline event reached DefaultBus (route trigger would never fire)")
	}
}
