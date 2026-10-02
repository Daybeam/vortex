package core

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/store"
)

const (
	chatEventBufferSize = 256
	maxSeenMsgEntries   = 10000 // cap on idempotency key set; clearing is safe (worst case: a very old retry gets reprocessed)
	maxCachedSessions   = 1000  // cap on in-memory session cache; sessions are persisted to disk/SQLite so eviction is safe
)

// ChatSession holds a chat thread's messages as a tree (map by ID) and its
// in-memory event ring buffer (ephemeral, used for SSE Last-Event-ID resume).
// Messages form a tree via ChatMessage.ParentID; ActiveLeafID points at the
// current branch tip. RootID is the first message (tree root).
type ChatSession struct {
	ID           string                  `json:"id"`
	Messages     map[string]*ChatMessage `json:"messages"`
	RootID       string                  `json:"root_id,omitempty"`
	ActiveLeafID string                  `json:"active_leaf_id,omitempty"`
	ActiveRoleID string                  `json:"active_role_id,omitempty"`
	CreatedAt    time.Time               `json:"created_at"`

	mu        sync.Mutex
	events    []ChatEvent
	nextSeq   int64
	seenMsg   map[string]bool
	dirtyMsgs []string // message IDs added since last Persist (audit PERF-6: avoid re-saving all messages)
	lastAccess time.Time
}

// GetActiveRoleID returns the session's active role ID (thread-safe).
// regression for audit LOGIC-1: ActiveRoleID was accessed without mutex.
func (s *ChatSession) GetActiveRoleID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ActiveRoleID
}

// SetActiveRoleID sets the session's active role ID (thread-safe).
// regression for audit LOGIC-1: ActiveRoleID was written without mutex.
func (s *ChatSession) SetActiveRoleID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ActiveRoleID = id
}

// AddEvent assigns the next sequence number and appends to the ring buffer.
func (s *ChatSession) AddEvent(ev ChatEvent) ChatEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSeq++
	ev.Seq = s.nextSeq
	s.events = append(s.events, ev)
	if len(s.events) > chatEventBufferSize {
		s.events = s.events[len(s.events)-chatEventBufferSize:]
	}
	return ev
}

// EventsAfter returns all events with Seq > lastSeq (for SSE resume).
func (s *ChatSession) EventsAfter(lastSeq int64) []ChatEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ChatEvent, 0, len(s.events))
	for _, e := range s.events {
		if e.Seq > lastSeq {
			out = append(out, e)
		}
	}
	return out
}

// AppendUserMessage adds a user message, deduplicating by messageID.
// parentID selects the branch point: empty means continue from ActiveLeafID.
// Returns false if messageID was already processed.
func (s *ChatSession) AppendUserMessage(messageID, content, parentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenMsg == nil {
		s.seenMsg = make(map[string]bool)
	}
	if s.Messages == nil {
		s.Messages = make(map[string]*ChatMessage)
	}
	if messageID != "" {
		if s.seenMsg[messageID] {
			return false
		}
		s.seenMsg[messageID] = true
		// Cap the idempotency key set to prevent unbounded growth.
		// Clearing is safe: the worst case is a very old retry gets
		// reprocessed, which just appends a duplicate message.
		if len(s.seenMsg) > maxSeenMsgEntries {
			s.seenMsg = make(map[string]bool)
			s.seenMsg[messageID] = true
		}
	}
	if parentID == "" {
		parentID = s.ActiveLeafID
	}
	msg := &ChatMessage{
		ID:        uuid.New().String(),
		ParentID:  parentID,
		Role:      "user",
		Content:   content,
		CreatedAt: time.Now(),
	}
	s.Messages[msg.ID] = msg
	s.dirtyMsgs = append(s.dirtyMsgs, msg.ID) // audit PERF-6: track new messages for incremental persist
	if s.RootID == "" {
		s.RootID = msg.ID
	}
	s.ActiveLeafID = msg.ID
	return true
}

// AppendMessage adds a non-user message (assistant / tool) as a child of
// the active leaf (or parentID if non-empty).
func (s *ChatSession) AppendMessage(msg ChatMessage, parentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Messages == nil {
		s.Messages = make(map[string]*ChatMessage)
	}
	if parentID == "" {
		parentID = s.ActiveLeafID
	}
	if msg.ID == "" {
		msg.ID = uuid.New().String()
	}
	msg.ParentID = parentID
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	m := msg
	s.Messages[m.ID] = &m
	s.dirtyMsgs = append(s.dirtyMsgs, m.ID) // audit PERF-6: track new messages for incremental persist
	if s.RootID == "" {
		s.RootID = m.ID
	}
	s.ActiveLeafID = m.ID
}

// getPathLocked walks the ParentID chain from msgID to root, returning
// the ordered path (root first). Caller must hold s.mu.
func (s *ChatSession) getPathLocked(msgID string) []ChatMessage {
	var path []ChatMessage
	cur := s.Messages[msgID]
	for cur != nil {
		path = append(path, *cur)
		if cur.ParentID == "" {
			break
		}
		cur = s.Messages[cur.ParentID]
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// GetPath returns the ordered message path from root to msgID.
func (s *ChatSession) GetPath(msgID string) []ChatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getPathLocked(msgID)
}

// ListBranches returns all message IDs that are direct children of parentID.
// Useful for discovering sibling branches ("information after the branch point").
func (s *ChatSession) ListBranches(parentID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var children []string
	for id, m := range s.Messages {
		if m.ParentID == parentID {
			children = append(children, id)
		}
	}
	return children
}

// SnapshotMessages returns the active branch path (root → ActiveLeafID).
func (s *ChatSession) SnapshotMessages() []ChatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ActiveLeafID == "" {
		return nil
	}
	return s.getPathLocked(s.ActiveLeafID)
}

// Clear resets the session to an empty state, removing all messages,
// events, and idempotency keys. nextSeq is preserved so that SSE
// Last-Event-ID resume continues to work correctly (new events after
// a clear will have Seq values higher than any pre-clear event).
func (s *ChatSession) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = make(map[string]*ChatMessage)
	s.RootID = ""
	s.ActiveLeafID = ""
	s.ActiveRoleID = ""
	s.events = nil
	s.seenMsg = make(map[string]bool)
	s.dirtyMsgs = nil // audit PERF-6: reset dirty tracking on clear
}

// ChatSessionStore owns in-memory sessions and persists each to a JSON file
// and/or SQLite backend (A20 DB collapse).
type ChatSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*ChatSession
	dir      string
	backend  store.IChatBackend

	// lifecycleCtx is used as the parent context for DB operations
	// (audit C-12). When nil, context.Background() is used.
	lifecycleCtx context.Context
}

func NewChatSessionStore(dir string, backend store.IChatBackend) *ChatSessionStore {
	if dir == "" {
		dir = filepath.Join("outputs", "chat")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("WARN: chat_session: failed to create session dir %s: %v", dir, err)
	}
	return &ChatSessionStore{sessions: make(map[string]*ChatSession), dir: dir, backend: backend}
}

// SetLifecycleContext sets the parent context for DB operations (audit C-12).
// When set, DB saves can be cancelled by shutdown via this context.
func (st *ChatSessionStore) SetLifecycleContext(ctx context.Context) {
	st.mu.Lock()
	st.lifecycleCtx = ctx
	st.mu.Unlock()
}

// dbCtx returns the context to use for DB operations (audit C-12).
func (st *ChatSessionStore) dbCtx() (context.Context, context.CancelFunc) {
	parent := st.lifecycleCtx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, 30*time.Second)
}

func (st *ChatSessionStore) GetOrCreate(id string) *ChatSession {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	if s, ok := st.sessions[id]; ok {
		s.lastAccess = now
		return s
	}
	s := &ChatSession{
		ID:         id,
		Messages:   make(map[string]*ChatMessage),
		CreatedAt:  now,
		seenMsg:    make(map[string]bool),
		lastAccess: now,
	}
	st.loadLocked(s)
	st.sessions[id] = s

	// Evict least-recently-accessed session if cache exceeds max size.
	// Sessions are persisted to disk/SQLite, so eviction just drops the
	// in-memory copy — the session can be reloaded on next access.
	if len(st.sessions) > maxCachedSessions {
		var oldestID string
		var oldestTime time.Time
		for sid, ss := range st.sessions {
			if oldestID == "" || ss.lastAccess.Before(oldestTime) {
				oldestID = sid
				oldestTime = ss.lastAccess
			}
		}
		delete(st.sessions, oldestID)
	}
	return s
}

func (st *ChatSessionStore) Get(id string) *ChatSession {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.sessions[id]
	if s != nil {
		s.lastAccess = time.Now()
	}
	return s
}

// ListSessions returns recent sessions from the backend (DB). If the DB
// returns no sessions, falls back to listing session JSON files from the
// directory. This handles the case where SaveSession silently fails (e.g.
// lifecycle context cancelled) but JSON files were still written.
func (st *ChatSessionStore) ListSessions(ctx context.Context, limit int) ([]store.ChatSessionRow, error) {
	st.mu.Lock()
	backend := st.backend
	dir := st.dir
	st.mu.Unlock()
	if backend != nil {
		rows, err := backend.ListSessions(ctx, limit)
		if err == nil && len(rows) > 0 {
			return rows, nil
		}
		// DB returned empty or error — fall through to file-based fallback
	}
	// File-based fallback: list *.json files in dir, sorted by mod time desc.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // dir doesn't exist or unreadable — not an error for caller
	}
	var rows []store.ChatSessionRow
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		rows = append(rows, store.ChatSessionRow{
			ID:        id,
			CreatedAt: info.ModTime(),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CreatedAt.After(rows[j].CreatedAt)
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// chatSessionData is the on-disk format for tree-based sessions.
type chatSessionData struct {
	Messages      map[string]*ChatMessage `json:"messages"`
	RootID        string                  `json:"root_id,omitempty"`
	ActiveLeafID  string                  `json:"active_leaf_id,omitempty"`
	ActiveRoleID  string                  `json:"active_role_id,omitempty"` // regression for audit LOGIC-5: was missing — role lost on restart
}

// Persist writes the session's tree to <dir>/<id>.json and/or SQLite backend.
func (st *ChatSessionStore) Persist(s *ChatSession) error {
	s.mu.Lock()
	// audit C-6: deep-copy messages map under lock. data.Messages = s.Messages
	// copies only the map header (same backing map), so post-unlock iteration
	// races with concurrent AppendUserMessage/AppendMessage map writes.
	msgCopy := make(map[string]*ChatMessage, len(s.Messages))
	for k, v := range s.Messages {
		msgCopy[k] = v
	}
	// audit PERF-6: only save new (dirty) messages to DB, not all messages.
	// The JSON file still gets the full snapshot (one write, not N).
	dirtyIDs := s.dirtyMsgs
	s.dirtyMsgs = nil
	data := chatSessionData{
		Messages:     msgCopy,
		RootID:       s.RootID,
		ActiveLeafID: s.ActiveLeafID,
		ActiveRoleID: s.ActiveRoleID, // regression for audit LOGIC-5: persist active role
	}
	s.mu.Unlock()

	// A20 DB collapse: save to SQLite backend when available
	if st.backend != nil {
		ctx, cancel := st.dbCtx()
		if err := st.backend.SaveSession(ctx, s.ID, s.RootID, s.ActiveLeafID, s.CreatedAt); err != nil {
			log.Printf("WARN: chat_session: SaveSession failed for %s: %v (file fallback will cover listing)", s.ID, err)
		}
		// audit PERF-6: was iterating ALL messages (O(T×M) over a conversation).
		// Now only saves messages added since last persist.
		for _, id := range dirtyIDs {
			if msg := msgCopy[id]; msg != nil {
				if err := st.backend.SaveMessage(ctx, msg.ID, s.ID, msg.ParentID, msg.Role, msg.Content, msg.CreatedAt); err != nil {
					log.Printf("WARN: chat_session: SaveMessage failed for %s/%s: %v", s.ID, msg.ID, err)
				}
			}
		}
		cancel()
	}

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(st.dir, s.ID+".json"), raw, 0644)
}

func (st *ChatSessionStore) loadLocked(s *ChatSession) {
	// A20 DB collapse: try SQLite backend first
	if st.backend != nil {
		ctx, cancel := st.dbCtx()
		defer cancel()
		rootID, leafID, msgs, err := st.backend.LoadSession(ctx, s.ID)
		cancel()
		if err == nil && len(msgs) > 0 {
			s.Messages = make(map[string]*ChatMessage)
			for id, row := range msgs {
				s.Messages[id] = &ChatMessage{
					ID:        row.ID,
					ParentID:  row.ParentID,
					Role:      row.Role,
					Content:   row.Content,
					CreatedAt: row.CreatedAt,
				}
			}
			s.RootID = rootID
			s.ActiveLeafID = leafID
			return
		}
	}

	data, err := os.ReadFile(filepath.Join(st.dir, s.ID+".json"))
	if err != nil {
		return
	}
	// Try tree-based format first.
	var tsd chatSessionData
	if json.Unmarshal(data, &tsd) == nil && tsd.Messages != nil {
		s.Messages = tsd.Messages
		s.RootID = tsd.RootID
		s.ActiveLeafID = tsd.ActiveLeafID
		s.ActiveRoleID = tsd.ActiveRoleID // regression for audit LOGIC-5: restore active role
		return
	}
	// Fallback: old flat array format — convert to tree with sequential IDs.
	var oldMsgs []ChatMessage
	if json.Unmarshal(data, &oldMsgs) != nil {
		return
	}
	s.Messages = make(map[string]*ChatMessage)
	var prevID string
	for i := range oldMsgs {
		m := oldMsgs[i]
		if m.ID == "" {
			m.ID = uuid.New().String()
		}
		m.ParentID = prevID
		if m.CreatedAt.IsZero() {
			m.CreatedAt = s.CreatedAt
		}
		s.Messages[m.ID] = &m
		if i == 0 {
			s.RootID = m.ID
		}
		prevID = m.ID
	}
	s.ActiveLeafID = prevID
}
