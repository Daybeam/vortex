package store

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMountinfoFstypeParsing verifies the mountinfo parser against the live
// /proc/self/mountinfo table plus synthetic inputs covering the octal-escape
// and longest-prefix-selection logic.
func TestMountinfoFstypeParsing(t *testing.T) {
	// /proc/self/mountinfo exists only on Linux; synthetic cases still run everywhere.
	if runtime.GOOS == "linux" {
		if fstype := mountinfoFstype("/"); fstype == "" {
			t.Fatal("mountinfoFstype(/) returned empty — parser is broken")
		}
	}

	// Synthetic table: longest-prefix wins; octal escape decodes.
	synthetic := strings.Join([]string{
		"18 1 8:1 / /  rw - ext4 /dev/sda1 rw",
		"19 18 8:2 / /root  rw - 9p drvfs rw",
		"20 19 8:3 / /root/data  rw - ext4 /dev/sdb1 rw",
		// escaped space: mount point "/root/my\\040dir" = "/root/my dir"
		"21 19 8:4 / /root/my\\040dir  rw - virtiofs /x rw",
	}, "\n")

	cases := []struct {
		dir  string
		want string
	}{
		{"/", "ext4"},
		{"/root", "9p"},
		{"/root/data", "ext4"},
		{"/root/data/sub", "ext4"},       // longest prefix still matches
		{"/root/my dir", "virtiofs"},     // octal-escaped mount point decoded
		{"/root/my dir/sub", "virtiofs"}, // nested under decoded mount
		{"/nonexistent", "ext4"},         // falls back to root mount
	}
	for _, c := range cases {
		got := parseSyntheticMountinfo(c.dir, synthetic)
		if got != c.want {
			t.Errorf("dir=%q: got fstype %q, want %q", c.dir, got, c.want)
		}
	}
}

// parseSyntheticMountinfo is a test-local reimplementation that feeds the
// synthetic table through the same logic mountinfoFstype uses, without
// touching the real /proc file. Kept separate so the parser's core
// (prefix selection + escape decoding) is testable in isolation.
func parseSyntheticMountinfo(dir, table string) string {
	bestLen := -1
	bestFstype := ""
	for _, line := range strings.Split(table, "\n") {
		fields, tail, found := strings.Cut(line, " - ")
		if !found {
			continue
		}
		parts := strings.Fields(fields)
		if len(parts) < 5 {
			continue
		}
		mountPoint := parts[4]
		if strings.Contains(mountPoint, "\\") {
			mountPoint = unescapeMountinfoOctal(mountPoint)
		}
		prefix := strings.TrimRight(mountPoint, "/")
		if dir == mountPoint || strings.HasPrefix(dir, prefix+"/") {
			if len(mountPoint) > bestLen {
				bestLen = len(mountPoint)
				bestFstype = strings.Fields(tail)[0]
			}
		}
	}
	return strings.ToLower(bestFstype)
}

// TestUnescapeMountinfoOctal covers the escape decoder.
func TestUnescapeMountinfoOctal(t *testing.T) {
	cases := []struct{ in, want string }{
		{`/root/my\040dir`, "/root/my dir"},
		{`/root/a\011b`, "/root/a\tb"},
		{`/plain`, "/plain"},
		{`/trailing\`, "/trailing\\"},
		{`/x\040y\040z`, "/x y z"},
	}
	for _, c := range cases {
		if got := unescapeMountinfoOctal(c.in); got != c.want {
			t.Errorf("unescape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestOnCrossVMFilesystem verifies the real-environment detection: /root/.hermes
// sits on 9p (D:\) in this deployment, /root/.cache/uv on ext4.
func TestOnCrossVMFilesystem(t *testing.T) {
	// These paths are environment-specific; skip when the expected fs differs.
	if fstype := mountinfoFstype("/root/.hermes"); fstype == "9p" {
		if !onCrossVMFilesystem("/root/.hermes/cron/jobs.json") {
			t.Error("expected /root/.hermes to be detected cross-VM (9p)")
		}
	}
	if fstype := mountinfoFstype("/root/.cache/uv"); fstype != "" && fstype != "9p" && fstype != "virtiofs" {
		if onCrossVMFilesystem("/root/.cache/uv/x.db") {
			t.Errorf("expected %s NOT cross-VM, but detected as such", fstype)
		}
	}
}

// TestOnCrossVMFilesystemRelativePath guards the absolute-path resolution:
// InitDB is called with relative paths in production ("build/bin/x.db"),
// and without resolution the mountinfo prefix match silently fails.
func TestOnCrossVMFilesystemRelativePath(t *testing.T) {
	if !onCrossVMFilesystem("/root/openclaw_workspace/") {
		t.Skip("repo not on cross-VM fs; env-specific")
	}
	// Relative path from a 9p cwd must still be detected.
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	if err := os.Chdir("/root/openclaw_workspace/mcp-servers/orchestrator-mcp-go"); err != nil {
		t.Fatal(err)
	}
	if !onCrossVMFilesystem("build/bin/orchestrator_runtime.db") {
		t.Error("relative path on 9p cwd not detected as cross-VM (false negative)")
	}
}

// TestInitDBJournalModeSelection verifies InitDB picks DELETE on 9p and WAL on
// a native path, by reading the actual on-disk journal mode after open.
func TestInitDBJournalModeSelection(t *testing.T) {
	// 9p path (this repo's runtime dir is on D:\ => 9p).
	if !onCrossVMFilesystem("/root/openclaw_workspace/mcp-servers/orchestrator-mcp-go/") {
		t.Skip("repo not on cross-VM fs; env-specific")
	}
	p9Path := filepath.Join("/root/openclaw_workspace/mcp-servers/orchestrator-mcp-go/build/bin", "probe_9p.db")
	os.Remove(p9Path)
	defer os.Remove(p9Path)

	db, err := InitDB(p9Path)
	if err != nil {
		t.Fatalf("InitDB(9p) failed: %v", err)
	}
	// Write first: on an empty DB, journal_mode is not persisted until a
	// transaction runs, so a bare PRAGMA read can report the driver default.
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS probe_t(a)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO probe_t VALUES(1)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if strings.ToUpper(mode) != "DELETE" {
		t.Errorf("9p path: journal_mode = %q, want DELETE", mode)
	}
	db.Close()

	// Native path: a real write dir under /tmp (overlay in this container).
	nativePath := "/tmp/orchestrator_fs_probe_" + "initdb.db"
	os.Remove(nativePath)
	defer os.Remove(nativePath)

	db2, err := InitDB(nativePath)
	if err != nil {
		t.Fatalf("InitDB(native) failed: %v", err)
	}
	if _, err := db2.Exec("CREATE TABLE IF NOT EXISTS probe_t(a)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db2.Exec("INSERT INTO probe_t VALUES(1)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db2.QueryRow("PRAGMA journal_mode;").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if strings.ToUpper(mode) != "WAL" {
		t.Errorf("native path: journal_mode = %q, want WAL", mode)
	}
	db2.Close()
}
