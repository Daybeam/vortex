package store

import (
	"encoding/json"
	"testing"
	"time"
)

// TestChatSessionRow_JSONTags reproduces the bug where ChatSessionRow had no
// JSON tags, causing PascalCase serialization (ID, CreatedAt) instead of the
// snake_case (id, created_at) the frontend expects.
//
// Bug: clicking a chat session in the History sidebar showed no content
// because the frontend accessed data.sessions[0].id (lowercase) but the API
// returned "ID" (PascalCase), resulting in undefined.
//
// Fix: added `json:"id"`, `json:"created_at"` etc. tags to ChatSessionRow.
func TestChatSessionRow_JSONTags(t *testing.T) {
	row := ChatSessionRow{
		ID:           "test-session-id",
		RootID:       "root-msg-id",
		ActiveLeafID: "leaf-msg-id",
		CreatedAt:    time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("json.Marshal error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal error: %v", err)
	}

	// Verify lowercase/snake_case keys exist
	requiredKeys := []string{"id", "created_at"}
	for _, key := range requiredKeys {
		if _, ok := m[key]; !ok {
			t.Errorf("missing key %q in JSON output. Got keys: %v", key, mapKeys(m))
		}
	}

	// Verify PascalCase keys do NOT exist (the bug)
	badKeys := []string{"ID", "RootID", "ActiveLeafID", "CreatedAt"}
	for _, key := range badKeys {
		if _, ok := m[key]; ok {
			t.Errorf("PascalCase key %q should not exist in JSON output. Got keys: %v", key, mapKeys(m))
		}
	}

	// Verify the id value is correct
	if m["id"] != "test-session-id" {
		t.Errorf("expected id='test-session-id', got %v", m["id"])
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
