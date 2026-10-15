package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verifyChain recomputes each entry's hash from its content and checks
// it matches the stored ThisHash. Returns true if the chain is intact.
func verifyChain(entries []AuditEntry) bool {
	prevHash := ""
	for _, e := range entries {
		if e.PrevHash != prevHash {
			return false
		}
		// Recompute hash from content (without ThisHash).
		copy := e
		copy.ThisHash = ""
		canonical, _ := json.Marshal(copy)
		h := sha256.Sum256([]byte(prevHash + string(canonical)))
		expected := hex.EncodeToString(h[:])
		if e.ThisHash != expected {
			return false // content was tampered
		}
		prevHash = e.ThisHash
	}
	return true
}

func TestAuditLogger_ChainIntegrity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	al := NewAuditLogger(path)

	// Write three entries.
	if err := al.Log("addDecision", "task1", "step1", "dec1", "step_failed", "", []string{"skip", "retry"}); err != nil {
		t.Fatalf("Log 1: %v", err)
	}
	if err := al.Log("submitDecision", "task1", "step1", "dec1", "step_failed", "retry", nil); err != nil {
		t.Fatalf("Log 2: %v", err)
	}
	if err := al.Log("autoResolve", "task2", "step2", "dec2", "upstream_insufficient", "skip", []string{"skip", "abort"}); err != nil {
		t.Fatalf("Log 3: %v", err)
	}

	// Read back and verify chain.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(lines))
	}

	var entries []AuditEntry
	for _, line := range lines {
		var e AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		entries = append(entries, e)
	}

	// Entry 0: genesis (prevHash = "")
	if entries[0].PrevHash != "" {
		t.Errorf("entry 0 prevHash should be empty (genesis), got %q", entries[0].PrevHash)
	}
	// Each subsequent entry's prevHash must match the previous entry's thisHash.
	for i := 1; i < len(entries); i++ {
		if entries[i].PrevHash != entries[i-1].ThisHash {
			t.Errorf("chain broken at entry %d: prevHash=%q, expected %q", i, entries[i].PrevHash, entries[i-1].ThisHash)
		}
	}
	// All entries should have non-empty hashes.
	for i, e := range entries {
		if e.ThisHash == "" {
			t.Errorf("entry %d has empty ThisHash", i)
		}
	}
}

func TestAuditLogger_TamperDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	al := NewAuditLogger(path)

	// audit NEW2-LOW-1: assert on setup Log errors for clear failure messages.
	if err := al.Log("addDecision", "task1", "step1", "dec1", "step_failed", "", []string{"skip"}); err != nil {
		t.Fatalf("setup Log 1 failed: %v", err)
	}
	if err := al.Log("submitDecision", "task1", "step1", "dec1", "step_failed", "skip", nil); err != nil {
		t.Fatalf("setup Log 2 failed: %v", err)
	}

	// Read and verify chain is initially intact.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("setup ReadFile failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var entries []AuditEntry
	for _, line := range lines {
		var e AuditEntry
		json.Unmarshal([]byte(line), &e)
		entries = append(entries, e)
	}
	if !verifyChain(entries) {
		t.Fatal("chain should be intact before tampering")
	}

	// Tamper: modify the first entry's action.
	entries[0].Action = "TAMPERED"
	tampered, _ := json.Marshal(entries[0])
	lines[0] = string(tampered)
	// audit NEW2-LOW-1: check tamper write error.
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatalf("tamper WriteFile failed: %v", err)
	}

	// Re-read and verify chain is now broken (recomputed hash won't match).
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read ReadFile failed: %v", err)
	}
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	entries = nil
	for _, line := range lines {
		var e AuditEntry
		json.Unmarshal([]byte(line), &e)
		entries = append(entries, e)
	}
	if verifyChain(entries) {
		t.Error("tamper not detected: chain appears intact after modifying entry 0's content")
	}
}

func TestAuditLogger_NilSafe(t *testing.T) {
	var al *AuditLogger
	// Should not panic.
	if err := al.Log("addDecision", "t", "s", "d", "", "", nil); err != nil {
		t.Errorf("nil AuditLogger.Log should return nil, got %v", err)
	}
}

func TestAuditLogger_RecoversChainAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	al1 := NewAuditLogger(path)
	// audit NEW2-LOW-1: assert on setup Log errors.
	if err := al1.Log("addDecision", "task1", "step1", "dec1", "step_failed", "", []string{"skip"}); err != nil {
		t.Fatalf("setup Log 1 failed: %v", err)
	}
	if err := al1.Log("submitDecision", "task1", "step1", "dec1", "step_failed", "skip", nil); err != nil {
		t.Fatalf("setup Log 2 failed: %v", err)
	}

	// Simulate restart: create a new AuditLogger pointing at the same file.
	al2 := NewAuditLogger(path)
	if al2.prevHash != al1.prevHash {
		t.Errorf("restart should recover prevHash: al2=%q, al1=%q", al2.prevHash, al1.prevHash)
	}
	// New entry should continue the chain.
	if err := al2.Log("autoResolve", "task2", "step2", "dec2", "", "abort", nil); err != nil {
		t.Fatalf("Log after restart failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 entries after restart, got %d", len(lines))
	}
	var e2, e3 AuditEntry
	json.Unmarshal([]byte(lines[1]), &e2)
	json.Unmarshal([]byte(lines[2]), &e3)
	if e3.PrevHash != e2.ThisHash {
		t.Error("chain broken after restart")
	}
}
