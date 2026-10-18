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
// C4: added TTL and capacity limits to prevent unbounded memory growth.
type DraftStore struct {
	mu          sync.Mutex
	drafts      map[string]*TaskDraft
	maxCapacity int
	ttl         time.Duration
}

// NewDraftStore initializes a new DraftStore with defaults: 100 drafts, 30min TTL.
func NewDraftStore() *DraftStore {
	return &DraftStore{
		drafts:      make(map[string]*TaskDraft),
		maxCapacity: 100,
		ttl:         30 * time.Minute,
	}
}

// Save stores or updates a task draft. C4: evicts expired drafts and
// enforces capacity by removing the oldest when full.
func (ds *DraftStore) Save(draft *TaskDraft) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	// Evict expired drafts
	now := time.Now()
	for id, d := range ds.drafts {
		if now.Sub(d.CreatedAt) > ds.ttl {
			delete(ds.drafts, id)
		}
	}

	// Enforce capacity: remove oldest if at limit
	if len(ds.drafts) >= ds.maxCapacity {
		var oldestID string
		var oldestTime time.Time
		for id, d := range ds.drafts {
			if oldestID == "" || d.CreatedAt.Before(oldestTime) {
				oldestID = id
				oldestTime = d.CreatedAt
			}
		}
		if oldestID != "" {
			delete(ds.drafts, oldestID)
		}
	}

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

func (ds *DraftStore) List() []*TaskDraft {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	result := make([]*TaskDraft, 0, len(ds.drafts))
	for _, d := range ds.drafts {
		result = append(result, d)
	}
	return result
}

func (s *DirectedEngine) GetDraft(draftID string) (*TaskDraft, bool) {
	if s.drafts == nil {
		return nil, false
	}
	return s.drafts.Get(draftID)
}

func (s *DirectedEngine) ListDrafts() []*TaskDraft {
	if s.drafts == nil {
		return nil
	}
	return s.drafts.List()
}

func (s *DirectedEngine) RemoveDraft(draftID string) {
	if s.drafts == nil {
		return
	}
	s.drafts.Remove(draftID)
}
