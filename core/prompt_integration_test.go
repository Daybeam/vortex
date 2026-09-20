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
	es, _ := store.NewExperienceStore("test_exp", ts, &reg.System, nil, nil)
	logger, _ := NewLogger("test_logs", &reg.System)
	defer logger.Close()

	spawner := NewSpawner(reg, ts, es, logger, NewResourceLoader(), "outputs")

	// Basic check that spawner can be initialized
	if spawner == nil {
		t.Fatal("failed to initialize spawner")
	}
}

/*
func TestPromptWarehouse_HotReloadIntegration(t *testing.T) {
    // Legacy test for removed PromptRef/PromptDir functionality
}
*/
