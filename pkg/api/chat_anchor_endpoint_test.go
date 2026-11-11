package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/pkg/chat"
)

func TestAnchorHandler_BasicLookup(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	for i := 0; i < 10; i++ {
		session.AppendUserMessage("", "msg", "", "")
		session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply"}, "")
	}

	path := session.SnapshotMessages()
	targetID := path[5].ID

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/history/anchor?session_id=s1&target_id="+targetID+"&limit=6", nil)
	w := httptest.NewRecorder()
	h.AnchorHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	msgs, _ := resp["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("expected 6 messages in anchor window, got %d", len(msgs))
	}
	if resp["has_more_older"] != true {
		t.Fatal("expected has_more_older=true (target is at index 5, window starts at 2)")
	}
	if resp["has_more_newer"] != true {
		t.Fatal("expected has_more_newer=true (target is at index 5, window ends at 7)")
	}
	oldestID, _ := resp["oldest_id"].(string)
	if oldestID == "" {
		t.Fatal("expected non-empty oldest_id")
	}
}

func TestAnchorHandler_TargetAtStart(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	for i := 0; i < 10; i++ {
		session.AppendUserMessage("", "msg", "", "")
		session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply"}, "")
	}

	path := session.SnapshotMessages()
	rootID := path[0].ID

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/history/anchor?session_id=s1&target_id="+rootID+"&limit=6", nil)
	w := httptest.NewRecorder()
	h.AnchorHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["has_more_older"] != false {
		t.Fatal("expected has_more_older=false (target is root)")
	}
}

func TestAnchorHandler_TargetAtEnd(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	for i := 0; i < 10; i++ {
		session.AppendUserMessage("", "msg", "", "")
		session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply"}, "")
	}

	path := session.SnapshotMessages()
	leafID := path[len(path)-1].ID

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/history/anchor?session_id=s1&target_id="+leafID+"&limit=6", nil)
	w := httptest.NewRecorder()
	h.AnchorHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["has_more_newer"] != false {
		t.Fatal("expected has_more_newer=false (target is leaf)")
	}
}

func TestAnchorHandler_TargetNotOnActivePath(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("u1", "msg1", "", "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply1"}, "")
	session.AppendUserMessage("u2", "msg2", "", "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply2"}, "")

	path := session.SnapshotMessages()
	branchPoint := path[1].ID
	session.AppendUserMessage("u3", "forked", branchPoint, "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "forked-reply"}, "")

	oldPath := session.SnapshotMessages()
	oldLeafID := oldPath[3].ID

	session.AppendUserMessage("u4", "back to old branch", oldLeafID, "")

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/history/anchor?session_id=s1&target_id=nonexistent_id", nil)
	w := httptest.NewRecorder()
	h.AnchorHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if _, hasError := resp["error"]; !hasError {
		t.Fatal("expected error for target not on active path")
	}
}

func TestAnchorHandler_MissingParams(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	h := &ChatHandler{Store: store}

	req := httptest.NewRequest(http.MethodGet, "/api/chat/history/anchor?session_id=s1", nil)
	w := httptest.NewRecorder()
	h.AnchorHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing target_id, got %d", w.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/chat/history/anchor?target_id=foo", nil)
	w2 := httptest.NewRecorder()
	h.AnchorHandler(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing session_id, got %d", w2.Code)
	}
}

func TestAnchorHandler_BranchPointsInWindow(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("u1", "msg1", "", "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply1"}, "")
	session.AppendUserMessage("u2", "msg2", "", "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply2"}, "")

	path := session.SnapshotMessages()
	branchPoint := path[1].ID
	session.AppendUserMessage("u3", "forked", branchPoint, "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "forked-reply"}, "")

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/history/anchor?session_id=s1&target_id="+branchPoint+"&limit=10", nil)
	w := httptest.NewRecorder()
	h.AnchorHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	bps, _ := resp["branch_points"].([]any)
	if len(bps) != 1 {
		t.Fatalf("expected 1 branch_point in anchor window, got %d", len(bps))
	}
}
