package core

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/verify"
	"github.com/daybeam/vortex/schemas"
)

// Q5-2 (2026-11-05): "没有真生产者就删链和面板。死 UI 比没有 UI 更糟."
//
// The SAV chain it refers to was Sense/Verify/Recover wrapped around every stdio
// MCP tool call. It could never run: config.Registry.GetVerifierID returned ""
// unconditionally, so the branch it fed was dead, and the only implementer of
// verify.Verifier in the repository was a test fake. The console panel that
// rendered its results keyed off metadata.sense / metadata.verify, which no
// producer ever wrote.
//
// This test does not prove the deletion works — the compiler proves that. It is
// a tripwire so the symbols cannot come back one line at a time: a new
// verifierRegistry field, or a GetVerifierID that returns "" again, would build
// fine and leave the next reader to rediscover the dead branch. Reintroducing
// either on purpose is legitimate — write the producer first and delete this
// file in the same commit that makes the branch reachable. See
// docs/audit/unwired-code-register.md §O8.

func TestSAVVerifierChainStaysDeleted(t *testing.T) {
	if _, ok := reflect.TypeOf(&config.Registry{}).MethodByName("GetVerifierID"); ok {
		t.Fatal(`config.Registry.GetVerifierID is back; without a tool->verifier mapping in config ` +
			`it returns "" and strands its caller (register §O8)`)
	}

	if _, ok := reflect.TypeOf(Spawner{}).FieldByName("verifierRegistry"); ok {
		t.Fatal("Spawner.verifierRegistry is back; nothing ever wrote to it, so every read missed")
	}
}

// TestDeterministicVerifierStillLive draws the line the deletion stops at:
// core/verify is not entirely dead code. RunDeterministicChecks is called by
// decision audit Case 2.5, so a future "finish the SAV cleanup" sweep must not
// take the package with it.
func TestDeterministicVerifierStillLive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("done"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	checks := []schemas.DeterministicCheck{{
		Type:   "file_exists",
		Params: map[string]any{"path": "report.md"},
	}}

	passed, msg, failType := verify.RunDeterministicChecks(context.Background(), dir, checks)
	if !passed {
		t.Fatalf("file_exists check failed (%s / %s); the live verifier chain is what broke, not SAV", msg, failType)
	}
}
