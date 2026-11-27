package chat

import (
	"context"
	"encoding/json"
	"fmt"
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
	maxChatMessages     = 5000  // audit P-MED-9: cap on in-memory messages; persisted to disk/SQLite so eviction is safe
)

// AwaitingReplyInfo records that a user message is awaiting an assistant
// reply. Set on turn start, cleared on turn completion. Persisted to the
// chat_pending_replies table so the startup scan can detect turns
// interrupted by a crash. See docs/completed/2026-11-21/CHAT_REPLY_FAULT_RECOVERY_PLAN.md.
type AwaitingReplyInfo struct {
	MessageID string    `json:"message_id"`
	Content   string    `json:"content"`
	StartedAt time.Time `json:"started_at"`
}

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

	// Live Co-pilot shared session fields (ADDED 2026-10-28).
	// OwnerID is the session owner's user ID. For shared sessions, guest
	// requests are routed through the owner's approval pipeline by overriding
	// AuthOwnerKey with this value. See docs/completed/2026-10-30/LIVE_COPILOT_SHARED_SESSION_DESIGN.md.
	OwnerID string `json:"owner_id,omitempty"`
	Shared  bool   `json:"shared,omitempty"`

	// AwaitingReply is non-nil while a user message is awaiting an assistant
	// reply. Persisted to chat_pending_replies for crash recovery.
	// See docs/completed/2026-11-21/CHAT_REPLY_FAULT_RECOVERY_PLAN.md.
	AwaitingReply *AwaitingReplyInfo `json:"awaiting_reply,omitempty"`

	mu         sync.Mutex
	events     []ChatEvent
	nextSeq    int64
	seenMsg    map[string]bool
	dirtyMsgs  []string // message IDs added since last Persist (audit PERF-6: avoid re-saving all messages)
	lastAccess time.Time
	// clientIDs maps caller-supplied message ids (web optimistic ids,
	// Telegram message ids) to stored message ids. Clients chain later
	// turns onto their own ids, so every inbound reference (parent_id,
	// reply_to) must be resolved through this map — otherwise the stored
	// tree contains dangling parents and the active path breaks on reload.
	// Rebuilt from the persisted client_id column on load.
	clientIDs map[string]string
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

// GetOwnerID returns the session owner's user ID (thread-safe).
// For shared sessions, this is used to override AuthOwnerKey for guest users.
func (s *ChatSession) GetOwnerID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.OwnerID
}

// SetOwnerID sets the session owner's user ID (thread-safe).
func (s *ChatSession) SetOwnerID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.OwnerID = id
}

// IsShared returns whether this session is shared with public users (thread-safe).
func (s *ChatSession) IsShared() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Shared
}

// SetShared marks the session as shared or not shared (thread-safe).
func (s *ChatSession) SetShared(shared bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Shared = shared
}

// GetAwaitingReply returns the pending reply info, or nil if no turn is
// outstanding (thread-safe).
func (s *ChatSession) GetAwaitingReply() *AwaitingReplyInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AwaitingReply == nil {
		return nil
	}
	cp := *s.AwaitingReply
	return &cp
}

// SetAwaitingReply marks that a user message is awaiting an assistant reply
// (thread-safe). Called on turn start.
func (s *ChatSession) SetAwaitingReply(info *AwaitingReplyInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AwaitingReply = info
}

// ClearAwaitingReply clears the pending reply mark (thread-safe). Called on
// turn completion.
func (s *ChatSession) ClearAwaitingReply() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AwaitingReply = nil
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
// replyTo references a quoted message (Mode 2, may be on any branch).
// All three references may be caller-supplied ids (web optimistic ids,
// Telegram message ids); they are resolved to stored ids via the session's
// clientIDs map before use.
// forkedFrom is computed here (not by the caller): when the resolved
// parentID is NOT the previous active leaf, the message forks the tree and
// parentID is recorded as the fork origin so the UI can draw a connector.
// Returns false if messageID was already processed.
func (s *ChatSession) AppendUserMessage(messageID, content, parentID, replyTo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenMsg == nil {
		s.seenMsg = make(map[string]bool)
	}
	if s.Messages == nil {
		s.Messages = make(map[string]*ChatMessage)
	}
	if s.clientIDs == nil {
		s.clientIDs = make(map[string]string)
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
			// audit P-MED-8: partial eviction instead of full clear to prevent cache stampede
			i := 0
			for k := range s.seenMsg {
				i++
				if i%4 == 0 {
					delete(s.seenMsg, k)
				}
			}
			s.seenMsg[messageID] = true
		}
	}
	// Resolve caller-supplied ids to stored ids BEFORE any topology
	// decision (fork detection below) and before persisting the links.
	parentID = s.resolveClientIDLocked(parentID)
	replyTo = s.resolveClientIDLocked(replyTo)
	// Mode 1 fork marker: appending onto anything other than the current
	// active leaf means the conversation jumped back to an earlier point.
	forkedFrom := ""
	if parentID != "" && parentID != s.ActiveLeafID {
		forkedFrom = parentID
	}
	if parentID == "" {
		parentID = s.ActiveLeafID
	}
	msg := &ChatMessage{
		ID:         uuid.New().String(),
		ClientID:   messageID,
		ParentID:   parentID,
		ReplyTo:    replyTo,
		ForkedFrom: forkedFrom,
		Role:       "user",
		Content:    content,
		CreatedAt:  time.Now(),
	}
	s.Messages[msg.ID] = msg
	if messageID != "" {
		s.clientIDs[messageID] = msg.ID
	}
	// audit P-MED-9: cap in-memory messages to prevent unbounded growth.
	// Messages are persisted to disk/SQLite, so eviction is safe.
	if len(s.Messages) > maxChatMessages {
		type msgTime struct {
			id string
			ts time.Time
		}
		all := make([]msgTime, 0, len(s.Messages))
		for id, m := range s.Messages {
			all = append(all, msgTime{id, m.CreatedAt})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].ts.Before(all[j].ts) })
		evict := len(all) / 4 // evict oldest 25%
		for i := 0; i < evict; i++ {
			delete(s.Messages, all[i].id)
		}
	}
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
	// Register the client-supplied id for non-user messages too. The web client
	// creates assistant messages with a deterministic ClientID ("asst_"+
	// <user message_id>) and quotes that id; without registration here
	// resolveClientIDLocked cannot map it to the persisted UUID, so every quote
	// to an assistant message breaks after a history reload (2026-11-22).
	if m.ClientID != "" {
		if s.clientIDs == nil {
			s.clientIDs = make(map[string]string)
		}
		s.clientIDs[m.ClientID] = m.ID
	}
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

// resolveClientIDLocked maps a caller-supplied message reference to the
// stored message id. Values that are already stored ids pass through
// unchanged; unknown values are returned as-is (cross-branch quotes and
// stale ids degrade gracefully in the UI). Caller must hold s.mu.
func (s *ChatSession) resolveClientIDLocked(ref string) string {
	if ref == "" {
		return ""
	}
	if _, ok := s.Messages[ref]; ok {
		return ref // already a stored id (e.g. after a history reload)
	}
	if stored, ok := s.clientIDs[ref]; ok {
		return stored
	}
	return ref
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

// GetMessage returns a copy of the message with the given ID, or nil if not found.
// Thread-safe. Used for exact reference lookup (e.g. reply-to).
func (s *ChatSession) GetMessage(msgID string) *ChatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.Messages[msgID]
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}

// MessageMeta is the lightweight metadata returned by GetMessagesMeta.
type MessageMeta struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Preview string `json:"preview"`
}

// GetMessagesMeta returns lightweight metadata (role + truncated preview)
// for a batch of message IDs. Messages not found in the session are omitted
// from the result. Thread-safe, single lock for the entire batch.
func (s *ChatSession) GetMessagesMeta(ids []string) []MessageMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []MessageMeta
	for _, id := range ids {
		m := s.Messages[id]
		if m == nil {
			continue
		}
		preview := m.Content
		if len(preview) > 60 {
			preview = preview[:60]
		}
		result = append(result, MessageMeta{
			ID:      m.ID,
			Role:    m.Role,
			Preview: preview,
		})
	}
	return result
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

// SnapshotMessagesWindow returns up to limit messages from the active branch
// path, starting from the newest. If before is non-empty, the window starts
// from the parent of the message with that ID (exclusive of before itself).
// Returns messages in chronological order (oldest first), whether more older
// messages exist beyond this window (hasMore), and the ID of the oldest
// message in the window (oldestID). If limit <= 0, returns all messages
// from the starting point to root.
func (s *ChatSession) SnapshotMessagesWindow(limit int, before string) (msgs []ChatMessage, hasMore bool, oldestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ActiveLeafID == "" {
		return nil, false, ""
	}
	startID := s.ActiveLeafID
	if before != "" {
		m := s.Messages[before]
		if m == nil {
			return nil, false, ""
		}
		startID = m.ParentID
		if startID == "" {
			return nil, false, ""
		}
	}
	var path []ChatMessage
	cur := s.Messages[startID]
	for cur != nil {
		path = append(path, *cur)
		if limit > 0 && len(path) >= limit {
			break
		}
		if cur.ParentID == "" {
			break
		}
		cur = s.Messages[cur.ParentID]
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	if len(path) > 0 {
		oldestID = path[0].ID
		hasMore = path[0].ParentID != ""
	}
	return path, hasMore, oldestID
}

// SnapshotMessagesAnchor returns a window of messages centered on targetID
// on the active branch path. If targetID is not on the active path, returns
// an error. The window contains up to limit messages, with the target at the
// center. Returns hasMoreOlder (messages exist before the window), hasMoreNewer
// (messages exist after the window), and oldestID (cursor for loading older).
func (s *ChatSession) SnapshotMessagesAnchor(targetID string, limit int) (msgs []ChatMessage, hasMoreOlder bool, hasMoreNewer bool, oldestID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ActiveLeafID == "" {
		return nil, false, false, "", fmt.Errorf("session has no active leaf")
	}
	path := s.getPathLocked(s.ActiveLeafID)
	targetIdx := -1
	for i, m := range path {
		if m.ID == targetID {
			targetIdx = i
			break
		}
	}
	if targetIdx == -1 {
		return nil, false, false, "", fmt.Errorf("target not on active path")
	}
	if limit <= 0 {
		limit = 50
	}
	half := limit / 2
	start := targetIdx - half
	if start < 0 {
		start = 0
	}
	end := targetIdx + half + (limit % 2) - 1
	if end >= len(path) {
		end = len(path) - 1
	}
	window := path[start : end+1]
	hasMoreOlder = start > 0
	hasMoreNewer = end < len(path)-1
	oldestID = window[0].ID
	return window, hasMoreOlder, hasMoreNewer, oldestID, nil
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
	s.AwaitingReply = nil
	s.events = nil
	s.seenMsg = make(map[string]bool)
	s.clientIDs = make(map[string]string)
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

// ListPendingReplies returns all sessions with an outstanding pending reply
// mark from the DB backend. Returns nil if no backend is configured.
// Used by the startup scan for crash recovery.
func (st *ChatSessionStore) ListPendingReplies(ctx context.Context) ([]store.PendingReplyRow, error) {
	st.mu.Lock()
	backend := st.backend
	st.mu.Unlock()
	if backend == nil {
		return nil, nil
	}
	return backend.ListPendingReplies(ctx)
}

// ClearPendingReply removes the pending reply mark for a session from the
// DB backend. No-op if no backend is configured.
func (st *ChatSessionStore) ClearPendingReply(ctx context.Context, sessionID string) error {
	st.mu.Lock()
	backend := st.backend
	st.mu.Unlock()
	if backend == nil {
		return nil
	}
	return backend.ClearPendingReply(ctx, sessionID)
}

func (st *ChatSessionStore) getOrCreateLocked(id, ownerID string) (*ChatSession, bool) {
	now := time.Now()
	if s, ok := st.sessions[id]; ok {
		s.lastAccess = now
		return s, false
	}
	s := &ChatSession{
		ID:         id,
		OwnerID:    ownerID,
		Messages:   make(map[string]*ChatMessage),
		CreatedAt:  now,
		seenMsg:    make(map[string]bool),
		lastAccess: now,
	}
	found := st.loadLocked(s)
	st.sessions[id] = s
	st.evictLocked()
	return s, !found
}

func (st *ChatSessionStore) evictLocked() {
	if len(st.sessions) <= maxCachedSessions {
		return
	}
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

// GetOrCreate is for internal/test/background callers that do not represent an
// authenticated resource request. Request paths use GetOrCreateOwned instead.
func (st *ChatSessionStore) GetOrCreate(id string) *ChatSession {
	st.mu.Lock()
	s, _ := st.getOrCreateLocked(id, "")
	st.mu.Unlock()
	return s
}

// GetOrCreateOwned atomically loads/creates a session and authorizes an existing
// one. authorize runs under the store lock, closing the load/check race. A new
// owner is never applied over persisted metadata.
// See docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md.
func (st *ChatSessionStore) GetOrCreateOwned(id, ownerID string, ownerRequired bool, authorize func(*ChatSession) error) (*ChatSession, error) {
	st.mu.Lock()
	s, created := st.getOrCreateLocked(id, ownerID)
	if created && ownerRequired && ownerID == "" {
		delete(st.sessions, id)
		st.mu.Unlock()
		return nil, fmt.Errorf("owner identity is required")
	}
	if authorize != nil {
		if err := authorize(s); err != nil {
			if created {
				delete(st.sessions, id)
			}
			st.mu.Unlock()
			return nil, err
		}
	}
	st.mu.Unlock()
	if created {
		if err := st.Persist(s); err != nil {
			log.Printf("WARN: chat_session: initial Persist failed for %s: %v", id, err)
		}
	}
	return s, nil
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

// Load returns an existing cached or persisted session without creating a
// placeholder. Read endpoints use this so an unknown ID remains a 404.
func (st *ChatSessionStore) Load(id string) *ChatSession {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s := st.sessions[id]; s != nil {
		s.lastAccess = time.Now()
		return s
	}
	s := &ChatSession{ID: id, Messages: make(map[string]*ChatMessage), CreatedAt: time.Now()}
	if !st.loadLocked(s) {
		return nil
	}
	s.seenMsg = make(map[string]bool)
	s.lastAccess = time.Now()
	st.sessions[id] = s
	st.evictLocked()
	return s
}

// ListSessions returns recent sessions from the backend (DB). If the DB
// returns no sessions, falls back to listing session JSON files from the
// directory. This handles the case where SaveSession silently fails (e.g.
// lifecycle context cancelled) but JSON files were still written.
func (st *ChatSessionStore) ListSessions(ctx context.Context, scope store.SessionScope, limit, offset int) ([]store.ChatSessionRow, error) {
	st.mu.Lock()
	backend := st.backend
	dir := st.dir
	st.mu.Unlock()
	if backend != nil {
		rows, err := backend.ListSessions(ctx, scope, limit, offset)
		if err == nil && len(rows) > 0 {
			return rows, nil
		}
		// DB returned empty or error — fall through using the SAME scope.
	}
	// File-based fallback: read ownership metadata and filter before pagination.
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
		row := store.ChatSessionRow{ID: id, CreatedAt: info.ModTime()}
		if raw, err := os.ReadFile(filepath.Join(dir, entry.Name())); err == nil {
			var data chatSessionData
			if json.Unmarshal(raw, &data) == nil {
				row.OwnerID = data.OwnerID
				row.Shared = data.Shared
				row.RootID = data.RootID
				row.ActiveLeafID = data.ActiveLeafID
			}
		}
		if !scope.All && row.OwnerID != scope.Owner {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].CreatedAt.After(rows[j].CreatedAt)
	})
	// Apply offset then limit
	if offset > 0 && offset < len(rows) {
		rows = rows[offset:]
	} else if offset > 0 {
		return []store.ChatSessionRow{}, nil
	}
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
	OwnerID       string                  `json:"owner_id,omitempty"`
	Shared        bool                    `json:"shared,omitempty"`
	AwaitingReply *AwaitingReplyInfo      `json:"awaiting_reply,omitempty"` // crash recovery: non-nil = turn interrupted
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
	// Snapshot AwaitingReply under lock for both JSON and DB persistence.
	awaiting := s.AwaitingReply
	data := chatSessionData{
		Messages:      msgCopy,
		RootID:        s.RootID,
		ActiveLeafID:  s.ActiveLeafID,
		ActiveRoleID:  s.ActiveRoleID, // regression for audit LOGIC-5: persist active role
		OwnerID:       s.OwnerID,
		Shared:        s.Shared,
		AwaitingReply: awaiting,
	}
	s.mu.Unlock()

	// A20 DB collapse: save to SQLite backend when available
	if st.backend != nil {
		ctx, cancel := st.dbCtx()
		if err := st.backend.SaveSession(ctx, s.ID, data.RootID, data.ActiveLeafID, data.OwnerID, data.Shared, s.CreatedAt); err != nil {
			log.Printf("WARN: chat_session: SaveSession failed for %s: %v (file fallback will cover listing)", s.ID, err)
		}
		// audit PERF-6: was iterating ALL messages (O(T×M) over a conversation).
		// Now only saves messages added since last persist.
		for _, id := range dirtyIDs {
			if msg := msgCopy[id]; msg != nil {
				row := &store.ChatMessageRow{
					ID:         msg.ID,
					SessionID:  s.ID,
					ParentID:   msg.ParentID,
					ClientID:   msg.ClientID,
					ReplyTo:    msg.ReplyTo,
					ForkedFrom: msg.ForkedFrom,
					Role:       msg.Role,
					Content:    msg.Content,
					CreatedAt:  msg.CreatedAt,
				}
				if err := st.backend.SaveMessage(ctx, row); err != nil {
					log.Printf("WARN: chat_session: SaveMessage failed for %s/%s: %v", s.ID, msg.ID, err)
				}
			}
		}
		// Sync pending reply mark to DB for crash recovery.
		// See docs/completed/2026-11-21/CHAT_REPLY_FAULT_RECOVERY_PLAN.md §3.4–3.5.
		if awaiting != nil {
			if err := st.backend.SavePendingReply(ctx, store.PendingReplyRow{
				SessionID: s.ID,
				MessageID: awaiting.MessageID,
				Content:   awaiting.Content,
				StartedAt: awaiting.StartedAt,
			}); err != nil {
				log.Printf("WARN: chat_session: SavePendingReply failed for %s: %v", s.ID, err)
			}
		} else {
			if err := st.backend.ClearPendingReply(ctx, s.ID); err != nil {
				log.Printf("WARN: chat_session: ClearPendingReply failed for %s: %v", s.ID, err)
			}
		}
		cancel()
	}

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	// Self-heal the session dir before writing. NewChatSessionStore calls
	// MkdirAll at init, but that is a one-shot — if it failed (only logged
	// a warning) or the dir was removed after init (temp cleanup, external
	// rm), every persist would fail with "path not found". MkdirAll is
	// idempotent (cheap no-op if the dir exists), so calling it here makes
	// persist robust across the session lifetime.
	if err := os.MkdirAll(st.dir, 0755); err != nil {
		return fmt.Errorf("create session dir %s: %w", st.dir, err)
	}
	return os.WriteFile(filepath.Join(st.dir, s.ID+".json"), raw, 0644)
}

func (st *ChatSessionStore) loadLocked(s *ChatSession) bool {
	// A20 DB collapse: try SQLite backend first.
	dbLoaded := false
	if st.backend != nil {
		ctx, cancel := st.dbCtx()
		rootID, leafID, ownerID, shared, msgs, err := st.backend.LoadSession(ctx, s.ID)
		cancel()
		if err == nil {
			s.RootID = rootID
			s.ActiveLeafID = leafID
			s.OwnerID = ownerID
			s.Shared = shared
			dbLoaded = true
			if len(msgs) > 0 {
				s.Messages = make(map[string]*ChatMessage, len(msgs))
				for id, row := range msgs {
					s.Messages[id] = &ChatMessage{
						ID:         row.ID,
						ParentID:   row.ParentID,
						ClientID:   row.ClientID,
						ReplyTo:    row.ReplyTo,
						ForkedFrom: row.ForkedFrom,
						Role:       row.Role,
						Content:    row.Content,
						CreatedAt:  row.CreatedAt,
					}
				}
				s.rebuildClientIDsLocked()
				return true
			}
		}
	}

	data, err := os.ReadFile(filepath.Join(st.dir, s.ID+".json"))
	if err != nil {
		return dbLoaded
	}
	// Try tree-based format first.
	var tsd chatSessionData
	if json.Unmarshal(data, &tsd) == nil && tsd.Messages != nil {
		s.Messages = tsd.Messages
		s.RootID = tsd.RootID
		s.ActiveLeafID = tsd.ActiveLeafID
		s.ActiveRoleID = tsd.ActiveRoleID // regression for audit LOGIC-5: restore active role
		if !dbLoaded {
			s.OwnerID = tsd.OwnerID
			s.Shared = tsd.Shared
		}
		s.AwaitingReply = tsd.AwaitingReply
		s.rebuildClientIDsLocked()
		return true
	}
	// Fallback: old flat array format — convert to tree with sequential IDs.
	var oldMsgs []ChatMessage
	if json.Unmarshal(data, &oldMsgs) != nil {
		return dbLoaded
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
	s.rebuildClientIDsLocked()
	return true
}

// rebuildClientIDsLocked recomputes the client-id → stored-id map from the
// loaded messages so references from clients keep resolving after a reload
// or restart. Caller must hold s.mu.
func (s *ChatSession) rebuildClientIDsLocked() {
	s.clientIDs = make(map[string]string, len(s.Messages))
	for id, m := range s.Messages {
		if m != nil && m.ClientID != "" {
			s.clientIDs[m.ClientID] = id
		}
	}
}
