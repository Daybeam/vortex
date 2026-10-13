package core

// Local verification: use probe config to directly call PreviewPrompt, check if the engine-assembled system prompt is empty.
// Run: go test ./core/ -run TestProbePreviewPrompt -v

import (
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestProbePreviewPrompt(t *testing.T) {
	cfgPath := os.Getenv("PROBE_CONFIG")
	if cfgPath == "" {
		t.Skip("PROBE_CONFIG not set")
	}
	reg, err := config.NewRegistry(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	_lg, _ := NewLogger(os.TempDir(), &reg.System)
	sp := NewSpawner(reg, nil, nil, _lg, NewResourceLoader(), "") // minimal dependencies
	out, err := sp.PreviewPrompt("customer_service_agent",
		"Handle this customer service conversation. The customer (user id noah_muller_9847) wants to book a flight from San Francisco to New York for three passengers.",
		map[string]any{})
	if err != nil {
		t.Fatalf("PreviewPrompt: %v", err)
	}
	t.Logf("assembled prompt length = %d", len(out))
	t.Logf("head 600: %.600s", out)
	if len(out) == 0 {
		t.Fatal("ERROR: assembled system prompt is empty — empty prompt root cause is in the assembly layer")
	}
}
