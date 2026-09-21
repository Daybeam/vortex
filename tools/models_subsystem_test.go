package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestModelsSubsystem_List(t *testing.T) {
	reg, err := config.NewRegistry(filepath.Join(t.TempDir(), "config_test.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	reg.Providers["test-p1"] = &config.ProviderConfig{
		Provider:     "openai",
		Model:        "gpt-4o",
		Capabilities: []string{"coding", "vision"},
	}

	app := &App{Registry: reg}
	RegisterSubsystems(app)

	sub, ok := registry.Subsystems["models"]
	if !ok {
		t.Fatalf("models subsystem not registered")
	}
	listAct, ok := sub.Actions["list"]
	if !ok {
		t.Fatalf("models.list action not registered")
	}

	res, err := listAct.Handler(context.Background(), app, nil)
	if err != nil {
		t.Fatalf("models.list handler: %v", err)
	}

	summary, ok := res.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map[string]any from models.list, got %T", res)
	}

	found := false
	for _, m := range summary {
		if m["id"] == "test-p1" {
			found = true
			if m["provider"] != "openai" {
				t.Errorf("expected provider openai, got %v", m["provider"])
			}
			if m["model"] != "gpt-4o" {
				t.Errorf("expected model gpt-4o, got %v", m["model"])
			}
			caps := m["capabilities"].([]string)
			if len(caps) != 2 || caps[0] != "coding" {
				t.Errorf("expected capabilities [coding, vision], got %v", caps)
			}
		}
	}

	if !found {
		t.Error("test-p1 not found in models.list output")
	}
}
