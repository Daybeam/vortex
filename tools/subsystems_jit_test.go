package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/jit"
)

// N3 regression: manual jit.create keeps its one-hour default when the
// reflection-oriented default_jit_ttl setting is zero.
func TestJitCreate_ZeroConfigUsesManualDefault(t *testing.T) {
	reg := &config.Registry{DynamicMCPs: make(map[string]*config.MCPDef)}
	app := &App{Registry: reg, JIT: jit.NewJITManager(reg, t.TempDir())}
	t.Cleanup(app.JIT.Close)
	registerJitSubsystem(app)

	action := registry.Subsystems["jit"].Actions["create"]
	if got := action.Parameters["ttl"]; !strings.Contains(got.(string), "default 3600") {
		t.Fatalf("ttl description = %v, want manual default 3600", got)
	}
	result, err := action.Handler(context.Background(), app, map[string]any{
		"script": "-- manual", "language": "lua",
	})
	if err != nil {
		t.Fatalf("jit.create: %v", err)
	}
	id := result.(map[string]any)["mcp_id"].(string)
	assertJITTTL(t, reg, id, time.Hour)
}

func assertJITTTL(t *testing.T, reg *config.Registry, id string, want time.Duration) {
	t.Helper()
	mcp := reg.GetMCP(id)
	if mcp == nil {
		t.Fatalf("JIT tool %q not registered", id)
	}
	remaining := time.Until(mcp.ExpiresAt)
	if remaining < want-time.Minute || remaining > want+time.Minute {
		t.Fatalf("TTL for %s = %s, want %s", id, remaining.Round(time.Second), want)
	}
}
