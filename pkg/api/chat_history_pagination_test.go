package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/pkg/chat"
)

func TestHistoryHandler_NoPagination_BackwardCompatible(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("u1", "msg1", "", "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply1"}, "")
	session.AppendUserMessage("u2", "msg2", "", "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply2"}, "")

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/history?session_id=s1", nil)
	w := httptest.NewRecorder()
	h.HistoryHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := resp["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if _, hasHasMore := resp["has_more"]; hasHasMore {
		t.Fatal("has_more should not be present without pagination params")
	}
	if _, hasOldest := resp["oldest_id"]; hasOldest {
		t.Fatal("oldest_id should not be present without pagination params")
	}
}

func TestHistoryHandler_WithLimit(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	for i := 0; i < 10; i++ {
		session.AppendUserMessage("", "msg", "", "")
		session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply"}, "")
	}

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/history?session_id=s1&limit=4", nil)
	w := httptest.NewRecorder()
	h.HistoryHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	msgs, _ := resp["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if resp["has_more"] != true {
		t.Fatal("expected has_more=true")
	}
	oldestID, _ := resp["oldest_id"].(string)
	if oldestID == "" {
		t.Fatal("expected non-empty oldest_id")
	}
}

func TestHistoryHandler_WithBeforeCursor(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	for i := 0; i < 10; i++ {
		session.AppendUserMessage("", "msg", "", "")
		session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply"}, "")
	}

	h := &ChatHandler{Store: store}

	req1 := httptest.NewRequest(http.MethodGet, "/api/chat/history?session_id=s1&limit=4", nil)
	w1 := httptest.NewRecorder()
	h.HistoryHandler(w1, req1)
	var resp1 map[string]any
	json.Unmarshal(w1.Body.Bytes(), &resp1)
	oldestID, _ := resp1["oldest_id"].(string)

	req2 := httptest.NewRequest(http.MethodGet, "/api/chat/history?session_id=s1&limit=4&before="+oldestID, nil)
	w2 := httptest.NewRecorder()
	h.HistoryHandler(w2, req2)
	var resp2 map[string]any
	json.Unmarshal(w2.Body.Bytes(), &resp2)
	msgs2, _ := resp2["messages"].([]any)
	if len(msgs2) != 4 {
		t.Fatalf("expected 4 messages in second page, got %d", len(msgs2))
	}
	if resp2["has_more"] != true {
		t.Fatal("expected has_more=true for second page")
	}
}

func TestHistoryHandler_BeforeIsRoot(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	session.AppendUserMessage("u1", "msg1", "", "")
	session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply1"}, "")

	path := session.SnapshotMessages()
	rootID := path[0].ID

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/history?session_id=s1&limit=10&before="+rootID, nil)
	w := httptest.NewRecorder()
	h.HistoryHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	msgs, _ := resp["messages"].([]any)
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages when before=root, got %d", len(msgs))
	}
	if resp["has_more"] != false {
		t.Fatal("expected has_more=false when before is root")
	}
}

func TestHistoryHandler_BranchPointsWindowed(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/api/chat/history?session_id=s1&limit=2", nil)
	w := httptest.NewRecorder()
	h.HistoryHandler(w, req)
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	bps, _ := resp["branch_points"].([]any)
	if len(bps) != 0 {
		t.Fatalf("expected 0 branch_points in newest window (fork point is older), got %d", len(bps))
	}

	oldestID, _ := resp["oldest_id"].(string)
	req2 := httptest.NewRequest(http.MethodGet, "/api/chat/history?session_id=s1&limit=2&before="+oldestID, nil)
	w2 := httptest.NewRecorder()
	h.HistoryHandler(w2, req2)
	var resp2 map[string]any
	json.Unmarshal(w2.Body.Bytes(), &resp2)
	bps2, _ := resp2["branch_points"].([]any)
	if len(bps2) != 1 {
		t.Fatalf("expected 1 branch_point in second window (the fork origin), got %d", len(bps2))
	}
}

func TestHistoryHandler_LimitCap(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	for i := 0; i < 5; i++ {
		session.AppendUserMessage("", "msg", "", "")
		session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply"}, "")
	}

	h := &ChatHandler{Store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/chat/history?session_id=s1&limit=999", nil)
	w := httptest.NewRecorder()
	h.HistoryHandler(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	msgs, _ := resp["messages"].([]any)
	if len(msgs) != 10 {
		t.Fatalf("limit=999 should be capped to 200 but return all 10 messages, got %d", len(msgs))
	}
}

func TestHistoryHandler_BeforeWithoutLimit(t *testing.T) {
	store := chat.NewChatSessionStore(t.TempDir(), nil)
	session := store.GetOrCreate("s1")
	for i := 0; i < 10; i++ {
		session.AppendUserMessage("", "msg", "", "")
		session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: "reply"}, "")
	}

	h := &ChatHandler{Store: store}

	req1 := httptest.NewRequest(http.MethodGet, "/api/chat/history?session_id=s1&limit=4", nil)
	w1 := httptest.NewRecorder()
	h.HistoryHandler(w1, req1)
	var resp1 map[string]any
	json.Unmarshal(w1.Body.Bytes(), &resp1)
	oldestID, _ := resp1["oldest_id"].(string)

	req2 := httptest.NewRequest(http.MethodGet, "/api/chat/history?session_id=s1&before="+oldestID, nil)
	w2 := httptest.NewRecorder()
	h.HistoryHandler(w2, req2)
	var resp2 map[string]any
	json.Unmarshal(w2.Body.Bytes(), &resp2)
	msgs2, _ := resp2["messages"].([]any)
	if len(msgs2) != 16 {
		t.Fatalf("before without limit should default to 50 but return all remaining 16, got %d", len(msgs2))
	}
	if resp2["has_more"] != false {
		t.Fatal("expected has_more=false (reached root)")
	}
}
