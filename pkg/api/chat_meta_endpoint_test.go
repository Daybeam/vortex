package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/pkg/chat"
)

func TestMetaHandler_BasicLookup(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("u1", "hello world this is a test message", "", "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "I can help with that"}, "")

	path := session.SnapshotMessages()
	userID := path[0].ID
	assistantID := path[1].ID

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/messages/meta?session_id=s1&ids="+userID+","+assistantID, nil)
	w := httptest.NewRecorder()
	h.MetaHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	meta, _ := resp["meta"].([]any)
	if len(meta) != 2 {
		t.Fatalf("expected 2 meta entries, got %d", len(meta))
	}
}

func TestMetaHandler_PreviewTruncation(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	longContent := "This is a very long message that should be truncated to 60 characters when returned as a preview"
	session.AppendUserMessage("u1", longContent, "", "")

	path := session.SnapshotMessages()
	msgID := path[0].ID

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/messages/meta?session_id=s1&ids="+msgID, nil)
	w := httptest.NewRecorder()
	h.MetaHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	meta, _ := resp["meta"].([]any)
	if len(meta) != 1 {
		t.Fatalf("expected 1 meta entry, got %d", len(meta))
	}
	entry, _ := meta[0].(map[string]any)
	preview, _ := entry["preview"].(string)
	if len(preview) != 60 {
		t.Fatalf("expected preview truncated to 60 chars, got %d: %q", len(preview), preview)
	}
	if preview != longContent[:60] {
		t.Fatalf("preview mismatch: got %q", preview)
	}
}

func TestMetaHandler_NotFoundIDs(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("u1", "hello", "", "")

	path := session.SnapshotMessages()
	realID := path[0].ID

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/messages/meta?session_id=s1&ids="+realID+",nonexistent1,nonexistent2", nil)
	w := httptest.NewRecorder()
	h.MetaHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	meta, _ := resp["meta"].([]any)
	if len(meta) != 1 {
		t.Fatalf("expected 1 meta entry (2 not found), got %d", len(meta))
	}
}

func TestMetaHandler_NoSessionID(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/messages/meta?ids=a,b", nil)
	w := httptest.NewRecorder()
	h.MetaHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	meta, _ := resp["meta"].([]any)
	if len(meta) != 0 {
		t.Fatalf("expected 0 meta entries without session_id, got %d", len(meta))
	}
}

func TestMetaHandler_NoIDs(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/messages/meta?session_id=s1", nil)
	w := httptest.NewRecorder()
	h.MetaHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	meta, _ := resp["meta"].([]any)
	if len(meta) != 0 {
		t.Fatalf("expected 0 meta entries without ids, got %d", len(meta))
	}
}

func TestMetaHandler_RoleField(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("u1", "user message", "", "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "assistant reply"}, "")

	path := session.SnapshotMessages()

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/messages/meta?session_id=s1&ids="+path[0].ID+","+path[1].ID, nil)
	w := httptest.NewRecorder()
	h.MetaHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	meta, _ := resp["meta"].([]any)
	if len(meta) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(meta))
	}
	entry0, _ := meta[0].(map[string]any)
	if entry0["role"] != "user" {
		t.Fatalf("expected role 'user' for first message, got %v", entry0["role"])
	}
	entry1, _ := meta[1].(map[string]any)
	if entry1["role"] != "assistant" {
		t.Fatalf("expected role 'assistant' for second message, got %v", entry1["role"])
	}
}
