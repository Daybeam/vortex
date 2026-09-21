package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// ToolInteraction holds one step's output in the task-scoped KV store.
type ToolInteraction struct {
	ToolName  string         `json:"tool_name"`
	Arguments map[string]any `json:"arguments"`
	Result    any            `json:"result"`
}

type StepResult struct {
	Data           any                  `json:"data"`
	Confidence     float64              `json:"confidence"`
	MissingContext []string             `json:"missing_context"`
	Capability     string               `json:"capability"`
	ProviderID     string               `json:"provider_id,omitempty"`
	ModelID        string               `json:"model_id,omitempty"`
	Attachments    []schemas.Attachment `json:"attachments"`
	Trace          []ToolInteraction    `json:"trace"`
	DecisionIDs    []string             `json:"decision_ids,omitempty"`   // Link to structured decisions
	StatesVisited  []string             `json:"states_visited,omitempty"` // PGPO: Visited environment states (ADDED 2026-09-08)
	CreatedAt      time.Time            `json:"created_at"`
}

// FileTaskBackend implements ITaskBackend using the local filesystem.
type FileTaskBackend struct {
	baseDir string
}

func NewFileTaskBackend(baseDir string) *FileTaskBackend {
	_ = os.MkdirAll(baseDir, 0755)
	return &FileTaskBackend{baseDir: baseDir}
}

func (b *FileTaskBackend) Save(ctx context.Context, taskID, stepID string, data []byte) error {
	dir := filepath.Join(b.baseDir, taskID)
	_ = os.MkdirAll(dir, 0755)
	return os.WriteFile(filepath.Join(dir, stepID+".json"), data, 0644)
}

func (b *FileTaskBackend) Load(ctx context.Context, taskID, stepID string) ([]byte, error) {
	return os.ReadFile(filepath.Join(b.baseDir, taskID, stepID+".json"))
}

func (b *FileTaskBackend) Delete(ctx context.Context, taskID string) (int, error) {
	dir := filepath.Join(b.baseDir, taskID)
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil
	}
	count := len(files)
	_ = os.RemoveAll(dir)
	return count, nil
}

func (b *FileTaskBackend) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	return true, nil
}

// TaskStore is a task-scoped KV store using an ITaskBackend.
type TaskStore struct {
	Mu      sync.RWMutex
	backend ITaskBackend
}

var _ ITaskStore = (*TaskStore)(nil)

func NewTaskStore(backend ITaskBackend) *TaskStore {
	return &TaskStore{backend: backend}
}

func (ts *TaskStore) Set(ctx context.Context, taskID, stepID string, result *StepResult) error {
	ts.Mu.Lock()
	defer ts.Mu.Unlock()

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return ts.backend.Save(ctx, taskID, stepID, data)
}

func (ts *TaskStore) Get(ctx context.Context, taskID, stepID string) (*StepResult, error) {
	ts.Mu.RLock()
	defer ts.Mu.RUnlock()

	data, err := ts.backend.Load(ctx, taskID, stepID)
	if err != nil {
		return nil, err
	}

	var res StepResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (ts *TaskStore) GetByRef(ctx context.Context, ref string) (*StepResult, error) {
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	return ts.Get(ctx, parts[0], parts[1])
}

func (ts *TaskStore) ClearTask(ctx context.Context, taskID string) (int, error) {
	ts.Mu.Lock()
	defer ts.Mu.Unlock()
	return ts.backend.Delete(ctx, taskID)
}

func (ts *TaskStore) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	ts.Mu.Lock()
	defer ts.Mu.Unlock()
	return ts.backend.Claim(ctx, taskID, stepID)
}
