package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

type mockBackend struct {
	data map[string][]byte
}

func (m *mockBackend) Save(ctx context.Context, taskID, stepID string, data []byte) error {
	m.data[taskID+":"+stepID] = data
	return nil
}
func (m *mockBackend) Load(ctx context.Context, taskID, stepID string) ([]byte, error) {
	d, ok := m.data[taskID+":"+stepID]
	if !ok {
		return nil, os.ErrNotExist
	}
	return d, nil
}
func (m *mockBackend) Delete(ctx context.Context, taskID string) (int, error) { return 0, nil }
func (m *mockBackend) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	return true, nil
}

func TestSpawner_MultimodalFallbackDetection(t *testing.T) {
	// 1. Setup Registry
	reg := &config.Registry{
		DefaultProvider: "main_model",
		Providers: map[string]*config.ProviderConfig{
			"text_only": {
				Provider:  "openai",
				Model:     "gpt-3.5-turbo",
				APIKeyEnv: "OPENAI_API_KEY",
				PoolID:    "text_only",
			},
			"main_model": {
				Provider:  "openai",
				Model:     "gpt-4o",
				APIKeyEnv: "OPENAI_API_KEY",
				PoolID:    "main_model",
			},
		},
		Roles: map[string]*config.Role{
			"vision_user": {
				ID:       "vision_user",
				Provider: "text_only",
			},
		},
		System: config.SystemSettings{
			ConfidenceThreshold: 0.7,
		},
	}

	// 2. Setup TaskStore with an attachment
	backend := &mockBackend{data: make(map[string][]byte)}
	ts := store.NewTaskStore(backend)

	taskID := "task1"
	stepID := "step1"

	// Mock CURRENT step with attachment (naive implementation current behavior)
	ts.Set(context.Background(), taskID, stepID, &store.StepResult{
		Attachments: []schemas.Attachment{
			{Path: "test.jpg", MimeType: "image/jpeg", Label: "test"},
		},
	})

	// 3. Setup Logger to capture events
	logDir := t.TempDir()
	logger, _ := NewLogger(logDir, &config.SystemSettings{})

	// 4. Setup Spawner using NewSpawner
	loader := &ResourceLoader{}
	s := NewSpawner(reg, ts, nil, logger, loader, "outputs")

	// 5. Trigger doSpawn (it will fail on provider call, but we check logs before that)
	ctx := context.Background()
	hub := NewContextHub(reg, nil, nil)

	req := &SpawnRequest{
		TaskID: taskID,
		StepID: stepID,
		RoleID: "vision_user",
		Task:   "What is in this image?",
		Hub:    hub,
	}

	// We expect this to fail eventually because providers.Get will fail,
	// but we want to see if it planned the fallback.
	_, _ = s.doSpawn(ctx, req)

	// Flush logs
	logger.Close()

	// 6. Check logs
	date := time.Now().UTC().Format("2006-01-02")
	logFile := filepath.Join(logDir, date, taskID+".jsonl")
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file %s: %v", logFile, err)
	}
	logStr := string(content)

	if !strings.Contains(logStr, "EventMultimodalFallbackPlanned") {
		t.Errorf("expected EventMultimodalFallbackPlanned in logs, but not found. Logs: %s", logStr)
	}
	if !strings.Contains(logStr, "gpt-4o") {
		t.Errorf("expected gpt-4o (main model) to be mentioned in logs as fallback. Logs: %s", logStr)
	}
}
