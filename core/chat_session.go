package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/daybeam/vortex/store"
	"github.com/google/uuid"
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
	CreatedAt    time.Time               `json:"created_at"`

	mu         sync.Mutex
	events     []ChatEvent
	nextSeq    int64
	seenMsg    map[string]bool
	lastAccess time.Time
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

// ChatSessionStore owns in-memory sessions and persists each to a JSON file
// and/or SQLite backend (A20 DB collapse).
type ChatSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*ChatSession
	dir      string
	backend  store.IChatBackend
}

func NewChatSessionStore(dir string, backend store.IChatBackend) *ChatSessionStore {
	if dir == "" {
		dir = filepath.Join("outputs", "chat")
	}
	os.MkdirAll(dir, 0755)
	return &ChatSessionStore{sessions: make(map[string]*ChatSession), dir: dir, backend: backend}
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

// chatSessionData is the on-disk format for tree-based sessions.
type chatSessionData struct {
	Messages     map[string]*ChatMessage `json:"messages"`
	RootID       string                  `json:"root_id,omitempty"`
	ActiveLeafID string                  `json:"active_leaf_id,omitempty"`
}

// Persist writes the session's tree to <dir>/<id>.json and/or SQLite backend.
func (st *ChatSessionStore) Persist(s *ChatSession) error {
	s.mu.Lock()
	data := chatSessionData{
		Messages:     s.Messages,
		RootID:       s.RootID,
		ActiveLeafID: s.ActiveLeafID,
	}
	s.mu.Unlock()

	// A20 DB collapse: save to SQLite backend when available
	if st.backend != nil {
		// audit M9: bounded context prevents DB op from hanging forever
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		st.backend.SaveSession(ctx, s.ID, s.RootID, s.ActiveLeafID, s.CreatedAt)
		for _, msg := range data.Messages {
			st.backend.SaveMessage(ctx, msg.ID, s.ID, msg.ParentID, msg.Role, msg.Content, msg.CreatedAt)
		}
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
		// audit M9: bounded context prevents DB op from hanging forever
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
