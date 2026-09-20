package core

import (
	"fmt"
	"sync"
	"time"
)

// TaskDraft represents a failed task submission saved in memory for quick patching.
type TaskDraft struct {
	DraftID      string         `json:"draft_id"`
	Steps        []any          `json:"steps"` // Raw or parsed step inputs
	SessionRoles map[string]any `json:"session_roles,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	LastError    string         `json:"last_error"`
	FailedStep   int            `json:"failed_step"`
}

// DraftStore provides thread-safe in-memory caching for failed task submissions.
type DraftStore struct {
	mu     sync.Mutex
	drafts map[string]*TaskDraft
}

// NewDraftStore initializes a new DraftStore.
func NewDraftStore() *DraftStore {
	return &DraftStore{
		drafts: make(map[string]*TaskDraft),
	}
}

// Save stores or updates a task draft.
func (ds *DraftStore) Save(draft *TaskDraft) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.drafts[draft.DraftID] = draft
}

// Get retrieves a draft by its ID.
func (ds *DraftStore) Get(draftID string) (*TaskDraft, bool) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d, ok := ds.drafts[draftID]
	return d, ok
}

// Remove deletes a draft once successfully committed.
func (ds *DraftStore) Remove(draftID string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	delete(ds.drafts, draftID)
}

// PatchStep updates a specific field in a step within the draft.
func (ds *DraftStore) PatchStep(draftID string, stepIndex int, field string, val any) (*TaskDraft, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	draft, ok := ds.drafts[draftID]
	if !ok {
		return nil, fmt.Errorf("draft %s not found", draftID)
	}

	if stepIndex < 0 || stepIndex >= len(draft.Steps) {
		return nil, fmt.Errorf("invalid step index %d (total steps: %d)", stepIndex, len(draft.Steps))
	}

	// Safely cast step to map[string]any for flexible patching
	stepMap, ok := draft.Steps[stepIndex].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("internal error: step format is invalid")
	}

	stepMap[field] = val
	draft.LastError = ""
	draft.FailedStep = -1

	return draft, nil
}
