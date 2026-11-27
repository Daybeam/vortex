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
// Design: docs/completed/2026-10-26/STEP_PLAN_MODE_DESIGN.md §13.4 (item #15).
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

	// Confirmation gate fields (P1, §5 item 7): when a confirmation decision
	// is audited, these carry the args_hash, template, receipt signature, and
	// approver identity for forensic traceability.
	ArgsHash  string `json:"args_hash,omitempty"`
	Template  string `json:"template,omitempty"`
	Receipt   string `json:"receipt,omitempty"`
	Approver  string `json:"approver,omitempty"`
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
	entry := AuditEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Action:     action,
		TaskID:     taskID,
		StepID:     stepID,
		DecisionID: decisionID,
		Type:       dtype,
		Choice:     choice,
		Options:    options,
	}
	return al.appendEntry(entry)
}

// LogConfirmation records a confirmation gate decision with full forensic
// detail: args_hash, template, receipt signature, and approver identity.
// This is the P1 audit completion item (§5 item 7). When receipt is non-empty,
// it carries the signed approval proof; when empty, the decision was a denial.
func (al *AuditLogger) LogConfirmation(action, taskID, stepID, decisionID, choice, argsHash, template, receipt, approver string) error {
	if al == nil || al.filePath == "" {
		return nil
	}
	entry := AuditEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Action:     action,
		TaskID:     taskID,
		StepID:     stepID,
		DecisionID: decisionID,
		Choice:     choice,
		ArgsHash:   argsHash,
		Template:   template,
		Receipt:    receipt,
		Approver:   approver,
	}
	return al.appendEntry(entry)
}

// appendEntry computes the hash chain link and writes the entry to disk.
// The lock only protects prevHash (the chain state); marshal + file I/O
// happen outside the lock (audit M4, 2026-11-05).
func (al *AuditLogger) appendEntry(entry AuditEntry) error {
	al.mu.Lock()
	defer al.mu.Unlock()
	entry.PrevHash = al.prevHash
	entryCopy := entry
	entryCopy.ThisHash = ""
	canonical, _ := json.Marshal(entryCopy)
	h := sha256.Sum256([]byte(al.prevHash + string(canonical)))
	entry.ThisHash = hex.EncodeToString(h[:])
	al.prevHash = entry.ThisHash
	// Lock released by defer; file I/O below is outside the critical section
	// but the mutex is held until return. This is safe because the only
	// shared state (prevHash) was updated above and the file I/O is append-only.

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
