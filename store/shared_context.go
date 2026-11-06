package store

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ContextEntryType classifies a shared-context entry's semantic kind.
type ContextEntryType string

const (
	// ContextTypeFAIL: "Approach X doesn't work because Y." Prevents other
	// agents from wasting budget retrying known failures.
	ContextTypeFAIL ContextEntryType = "FAIL"

	// ContextTypeFACT: "Confirmed behavior: X." Prevents redundant exploration
	// of already-established truths.
	ContextTypeFACT ContextEntryType = "FACT"

	// ContextTypeOBSERVED: "Noticed: X." Lightweight observation, no strong
	// claim.
	ContextTypeOBSERVED ContextEntryType = "OBSERVED"

	// ContextTypeCLAIM: "I'm working on scope X." Prevents duplicate scope
	// claims. Scope overlap triggers async DM negotiation (Phase 2).
	ContextTypeCLAIM ContextEntryType = "CLAIM"

	// ContextTypePATCHSUMMARY: "Modified X. Reason: Y. Evidence: Z." Lets
	// downstream agents build on established changes without re-reading diffs.
	ContextTypePATCHSUMMARY ContextEntryType = "PATCH_SUMMARY"
)

// SharedContextEntry represents a single typed knowledge entry written by an
// agent during execution. Entries are append-only and scoped to a single
// task/execution context.
type SharedContextEntry struct {
	ID        string           `json:"id"`
	Type      ContextEntryType `json:"type"`
	Content   string           `json:"content"`
	Detail    string           `json:"detail,omitempty"`
	Author    string           `json:"author"`
	TaskID    string           `json:"task_id"`
	StepID    string           `json:"step_id,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
}

// SharedContext is the append-only knowledge board. Scoped per task.
// Thread-safe: concurrent writers from multiple spawner goroutines.
//
// Persistence: JSONL file at {dir}/{taskID}.jsonl. Each WriteEntry appends
// one line. Reads parse the full file and return the requested subset.
type SharedContext struct {
	mu sync.RWMutex
	// writeMu serialises the append path only. `mu` stopped covering file I/O
	// in audit M7 (2026-11-05), which is right for readers — they should not
	// queue behind a write — but leaves two writers appending to the same file
	// concurrently. O_APPEND is not atomic across goroutines here: the test
	// TestSharedContext_ConcurrentWrite lost 19 of 20 entries, and because
	// ReadAll skips lines it cannot parse, the loss was silent. Writers take
	// this lock; readers still only need RLock on `mu`.
	writeMu sync.Mutex
	dir     string // persistence directory
	taskID  string // scoping key; filename is {taskID}.jsonl
	seq     int64  // in-memory sequence counter for entry IDs
}

// NewSharedContext creates a SharedContext backed by a JSONL file in dir.
// The file is named {taskID}.jsonl. If dir does not exist it is created.
// A nil dir means in-memory only (entries are not persisted).
func NewSharedContext(dir, taskID string) (*SharedContext, error) {
	sc := &SharedContext{dir: dir, taskID: taskID}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("shared context: mkdir %s: %w", dir, err)
		}
	}
	return sc, nil
}

// filePath returns the JSONL persistence path, or "" if in-memory.
func (sc *SharedContext) filePath() string {
	if sc.dir == "" {
		return ""
	}
	return filepath.Join(sc.dir, sc.taskID+".jsonl")
}

// WriteEntry appends a new entry to the shared context. The entry's ID and
// CreatedAt are auto-filled if zero. Returns the written entry (with ID set).
func (sc *SharedContext) WriteEntry(entry *SharedContextEntry) (*SharedContextEntry, error) {
	// audit M7 (2026-11-05): formerly held mu.Lock across json.Marshal +
	// os.OpenFile + f.Write. The lock only protects sc.seq (the sequence
	// counter). Narrow the lock to just the seq increment + ID assignment,
	// then marshal + file I/O outside the lock.
	sc.mu.Lock()
	sc.seq++
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("ctx_%s_%d", sc.taskID, sc.seq)
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	entry.TaskID = sc.taskID
	sc.mu.Unlock()

	path := sc.filePath()
	if path == "" {
		return entry, nil // in-memory only
	}

	// One writer at a time, and only around the append: see the writeMu comment
	// on the struct. Readers do not take this lock.
	sc.writeMu.Lock()
	defer sc.writeMu.Unlock()

	line, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("shared context: marshal: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("shared context: open %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("shared context: write: %w", err)
	}
	return entry, nil
}

// ReadRecent returns the last limit entries (most recent first).
// If limit <= 0, defaults to 10. If typeFilter is non-empty, only entries
// of that type are returned.
func (sc *SharedContext) ReadRecent(limit int, typeFilter ContextEntryType) ([]*SharedContextEntry, error) {
	if limit <= 0 {
		limit = 10
	}
	all, err := sc.ReadAll()
	if err != nil {
		return nil, err
	}

	// Filter by type if requested.
	var filtered []*SharedContextEntry
	for _, e := range all {
		if typeFilter != "" && e.Type != typeFilter {
			continue
		}
		filtered = append(filtered, e)
	}

	// Return most recent first, up to limit.
	n := len(filtered)
	start := n - limit
	if start < 0 {
		start = 0
	}
	result := make([]*SharedContextEntry, 0, n-start)
	for i := n - 1; i >= start; i-- {
		result = append(result, filtered[i])
	}
	return result, nil
}

// ReadAll returns all entries in chronological order (oldest first).
func (sc *SharedContext) ReadAll() ([]*SharedContextEntry, error) {
	// audit H5: ReadAll only reads the append-only JSONL file (no shared
	// mutable state besides seq, which is written only under Lock). Use RLock
	// so concurrent readers don't block each other; was Lock, serializing
	// every read behind the writer.
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	path := sc.filePath()
	if path == "" {
		return nil, nil // in-memory only, no persistence
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no entries yet
		}
		return nil, fmt.Errorf("shared context: read %s: %w", path, err)
	}

	var entries []*SharedContextEntry
	corrupt := 0
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var e SharedContextEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// Skip malformed lines rather than failing the entire read, but say
			// so: an unparsable line means this file lost an entry, and the skip
			// alone made a concurrent-write regression look like "the test wrote
			// fewer entries" (see writeMu).
			corrupt++
			continue
		}
		entries = append(entries, &e)
	}
	if corrupt > 0 {
		log.Printf("[shared-context] %s: skipped %d unparsable line(s); those entries are lost",
			path, corrupt)
	}
	return entries, nil
}

// splitLines splits a byte slice into lines (each without the trailing newline).
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}
