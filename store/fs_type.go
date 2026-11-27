package store

import (
	"os"
	"path/filepath"
	"strings"
)

// crossVMFSTypes are filesystems that expose a host bind mount across a VM
// boundary (Docker Desktop / OrbStack / Podman: virtiofs or 9p). SQLite WAL's
// -shm file must be coherent shared memory for every opener; across the VM
// boundary it is NOT, and under write pressure the failure is SILENT
// (zero-filled pages). WAL must therefore be refused before the pragma.
// Port of hermes_state_wal.py:_CROSS_VM_FSTYPES (openclaw/openclaw#120597).
var crossVMFSTypes = map[string]bool{
	"virtiofs":      true,
	"fuse.virtiofs": true,
	"9p":            true,
	"9p2000":        true,
	"9p2000.l":      true,
	"9p2000.u":      true,
}

// mountinfoFstype returns the fstype of the longest mount point that is a
// prefix of dir ("" if /proc/self/mountinfo is unreadable or no match).
// Port of hermes_state_wal.py:_mountinfo_fstype.
func mountinfoFstype(dir string) string {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	bestLen := -1
	bestFstype := ""
	for _, line := range strings.Split(string(data), "\n") {
		// proc(5): <id> <parent> <maj:min> <root> <mount point> <opts> [optional...] - <fstype> <source> <super opts>
		fields, tail, found := strings.Cut(line, " - ")
		if !found {
			continue
		}
		parts := strings.Fields(fields)
		if len(parts) < 5 {
			continue
		}
		mountPoint := parts[4]
		if strings.Contains(mountPoint, "\\") { // octal escapes (\040 = space)
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

// unescapeMountinfoOctal decodes the \NNN octal escapes that /proc/self/mountinfo
// uses for whitespace and backslash in mount points (e.g. \040 = space).
func unescapeMountinfoOctal(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			// parse up to 3 octal digits
			var v int
			j := i + 1
			ok := true
			for j < len(s) && j < i+4 && s[j] >= '0' && s[j] <= '7' {
				v = v*8 + int(s[j]-'0')
				j++
			}
			if ok && j > i+1 {
				b.WriteByte(byte(v))
				i = j - 1
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// onCrossVMFilesystem reports whether path resides on a virtiofs/9p (cross-VM)
// filesystem. Only Linux /proc/self/mountinfo is consulted; on other platforms,
// or when the table is unreadable, the answer is false and behaviour is
// unchanged. Nothing else (ext4/btrfs/xfs/zfs/tmpfs/overlay/nfs) is flagged.
// Port of hermes_state_wal.py:_path_on_cross_vm_fs.
func onCrossVMFilesystem(path string) bool {
	// Resolve to an absolute path first: mountinfo prefixes are absolute
	// ("/root/..."), so a bare relative path ("build/bin/x.db") would never
	// match and would silently report "not cross-VM" — a false negative that
	// defeats the whole guard. Relative paths are resolved against cwd.
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "/"
	}
	fstype := mountinfoFstype(dir)
	return crossVMFSTypes[fstype]
}
