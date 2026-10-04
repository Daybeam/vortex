package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/store"
)

func TestPromptWarehouse_Integration(t *testing.T) {
	root, _ := os.Getwd()
	scenarioDir := filepath.Join(root, "../tests/scenarios/prompt_warehouse")
	configPath := filepath.Join(scenarioDir, "config.json")

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Skip("scenario config not found, skipping integration test")
		return
	}

	ts := store.NewTaskStore(store.NewFileTaskBackend("test_tasks"))
	es := mustNewExperienceStore(t, "test_exp", ts, &reg.System, nil, nil)
	logger := mustNewLogger(t, "test_logs", &reg.System)
	defer logger.Close()

	spawner := NewSpawner(reg, ts, es, logger, NewResourceLoader(), "outputs")

	// fixes audit T-C24: the original test only checked spawner != nil.
	// Now we verify the spawner was wired with the registry's providers.
	if spawner == nil {
		t.Fatal("failed to initialize spawner")
	}
	if spawner.registry == nil {
		t.Fatal("spawner registry not wired — integration incomplete")
	}
	if spawner.registry.DefaultProvider != reg.DefaultProvider {
		t.Errorf("spawner registry mismatch: got %q, want %q",
			spawner.registry.DefaultProvider, reg.DefaultProvider)
	}
}
