package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// AuditLogger records Decision Gate events in a tamper-evident SHA-256 hash
// chain. Each entry's hash incorporates the previous entry's hash, so any
// modification or deletion of a historical entry breaks the chain and is
// detectable on verification.
//
// Design: docs/plan/STEP_PLAN_MODE_DESIGN.md §13.4 (item #15).
// Only Decision Gate events are recorded (addDecision, SubmitDecision,
// resolveNonInteractiveDecision) — NOT every tool call, which would be
// too high-volume (Sieve already covers tool-level logging).
type AuditLogger struct {
	mu       sync.Mutex
	filePath string
	prevHash string // hash of the last written entry; "" = genesis
}

// AuditEntry is one record in the audit hash chain.
type AuditEntry struct {
	Timestamp  string   `json:"ts"`
	Action     string   `json:"action"` // "addDecision" | "submitDecision" | "autoResolve"
	TaskID     string   `json:"task_id"`
	StepID     string   `json:"step_id"`
	DecisionID string   `json:"decision_id"`
	Type       string   `json:"type,omitempty"`
	Choice     string   `json:"choice,omitempty"`
	Options    []string `json:"options,omitempty"`
	PrevHash   string   `json:"prev_hash"`
	ThisHash   string   `json:"hash"`
}

// NewAuditLogger creates an AuditLogger that appends to filePath.
// If the file already exists, it reads the last line to recover prevHash
// so the chain continues across restarts. If the file doesn't exist or
// can't be read, the chain starts fresh (genesis).
func NewAuditLogger(filePath string) *AuditLogger {
	al := &AuditLogger{filePath: filePath}
	// Recover chain state from existing file (best-effort).
	if data, err := os.ReadFile(filePath); err == nil && len(data) > 0 {
		// Find the last non-empty line.
		lines := splitLastLine(string(data))
		if lines != "" {
			var entry AuditEntry
			if json.Unmarshal([]byte(lines), &entry) == nil {
				al.prevHash = entry.ThisHash
			}
		}
	}
	return al
}

// Log records a single audit entry and appends it to the chain file.
// Safe for concurrent use. Errors are returned but callers typically ignore
// them (audit logging is best-effort — a failed append must not break the
// decision gate flow).
func (al *AuditLogger) Log(action, taskID, stepID, decisionID string, dtype, choice string, options []string) error {
	if al == nil || al.filePath == "" {
		return nil
	}
	al.mu.Lock()
	defer al.mu.Unlock()

	entry := AuditEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Action:     action,
		TaskID:     taskID,
		StepID:     stepID,
		DecisionID: decisionID,
		Type:       dtype,
		Choice:     choice,
		Options:    options,
		PrevHash:   al.prevHash,
	}
	// Compute hash over the entry WITHOUT ThisHash (which doesn't exist yet).
	entryCopy := entry
	entryCopy.ThisHash = ""
	canonical, _ := json.Marshal(entryCopy)
	h := sha256.Sum256([]byte(al.prevHash + string(canonical)))
	entry.ThisHash = hex.EncodeToString(h[:])

	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	f, err := os.OpenFile(al.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return err
	}
	al.prevHash = entry.ThisHash
	return nil
}

// SetAuditLogger wires a tamper-evident audit chain into the scheduler.
// Call once after NewDirectedEngine; nil-safe (passing nil disables logging).
func (s *DirectedEngine) SetAuditLogger(al *AuditLogger) {
	s.auditLogger = al
}

// splitLastLine returns the last non-empty line from a string.
func splitLastLine(data string) string {
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			rest := data[:i]
			// Find the start of this line.
			for j := len(rest) - 1; j >= 0; j-- {
				if rest[j] == '\n' {
					return rest[j+1:]
				}
			}
			return rest
		}
	}
	return data
}
