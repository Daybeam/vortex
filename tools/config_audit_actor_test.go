package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

// O4 (audit 2026-11-04): Invoke() stripped every _-prefixed argument before the
// handler ran, and nothing injected the identity server-side, so all eight
// strArg(args, "_actor") reads in subsystems_config.go fell through to the
// "admin" default. Every config change reached the WAL attributed to "admin".
// These tests pin the chain end to end: authenticated caller identity in,
// commit record out.

func newWALEnabledRegistry(t *testing.T) (*App, string) {
	t.Helper()
	t.Setenv("VORTEX_WAL_ENABLED", "true")
	path := filepath.Join(t.TempDir(), "config_test.json")
	reg, err := config.NewRegistry(path)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	app := &App{Registry: reg}
	RegisterSubsystems(app)
	return app, path + ".wal"
}

func lastWALCommit(t *testing.T, walPath string) config.Commit {
	t.Helper()
	f, err := os.Open(walPath)
	if err != nil {
		t.Fatalf("open WAL %s: %v", walPath, err)
	}
	defer f.Close()
	var last config.Commit
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			t.Fatalf("decode WAL line %q: %v", sc.Text(), err)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan WAL: %v", err)
	}
	return last
}

// TestO4_ConfigAuditActorIsTheAuthenticatedOwner is the regression test: an
// HTTP/SSE caller resolved to OwnerID "tenant-a" must be recorded as tenant-a,
// not "admin". A forged _actor/_task_id in the tool args must not survive.
func TestO4_ConfigAuditActorIsTheAuthenticatedOwner(t *testing.T) {
	app, walPath := newWALEnabledRegistry(t)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)
	ctx = context.WithValue(ctx, AuthOwnerKey, "tenant-a")

	_, err := registry.Invoke(ctx, app, "config", "update_system", map[string]any{
		"staging_enabled": true,
		"_actor":          "attacker-controlled",
		"_task_id":        "attacker-controlled",
	})
	if err != nil {
		t.Fatalf("Invoke update_system: %v", err)
	}

	commit := lastWALCommit(t, walPath)
	if commit.Actor != "tenant-a" {
		t.Errorf("WAL actor = %q, want %q -- the audit trail attributes the change to someone other than the caller it authorized",
			commit.Actor, "tenant-a")
	}
	if commit.TaskID != "" {
		t.Errorf("WAL task_id = %q, want empty: no transport carries a task id, so a value here means a caller forged one", commit.TaskID)
	}
}

// TestO4_LocalAdminStillRecordsAdmin guards the other direction: with no named
// key and no per-connection session (stdio, desktop, dev) the actor stays the
// documented "admin" default rather than becoming an empty string.
func TestO4_LocalAdminStillRecordsAdmin(t *testing.T) {
	app, walPath := newWALEnabledRegistry(t)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)

	if _, err := registry.Invoke(ctx, app, "config", "update_system", map[string]any{"staging_enabled": true}); err != nil {
		t.Fatalf("Invoke update_system: %v", err)
	}
	if got := lastWALCommit(t, walPath).Actor; got != "admin" {
		t.Errorf("WAL actor = %q, want the %q default for an owner-less local caller", got, "admin")
	}
}

// TestO4_DirectGoCallerKeepsReservedKeys is the sibling of the check in
// subsystems_update_system_test.go:156, but through the WAL. Direct Go callers
// bypass Invoke(), so they remain the only producer of _task_id (compare
// core/role_generator.go:209) and must keep working.
func TestO4_DirectGoCallerKeepsReservedKeys(t *testing.T) {
	app, walPath := newWALEnabledRegistry(t)

	action, ok := registry.Subsystems["config"].Actions["update_system"]
	if !ok {
		t.Fatalf("update_system action not registered")
	}
	if _, err := action.Handler(context.Background(), app, map[string]any{
		"staging_enabled": true, "_actor": "role_generator", "_task_id": "task-9",
	}); err != nil {
		t.Fatalf("direct handler call: %v", err)
	}

	commit := lastWALCommit(t, walPath)
	if commit.Actor != "role_generator" || commit.TaskID != "task-9" {
		t.Errorf("WAL recorded actor=%q task_id=%q, want role_generator/task-9", commit.Actor, commit.TaskID)
	}
}
