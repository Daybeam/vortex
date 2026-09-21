package config

import (
	"reflect"
	"runtime"
	"testing"
)

// TestResolveForPlatform_NilOverrides_ReturnsBaseFieldsUnchanged is the
// critical backward-compatibility guarantee: every MCP that doesn't set
// PlatformOverrides (i.e. every MCP defined before 2026-08-19, and any
// future MCP that only needs to run on one OS) must behave byte-for-byte
// identically to before this field existed.
func TestResolveForPlatform_NilOverrides_ReturnsBaseFieldsUnchanged(t *testing.T) {
	m := &MCPDef{
		Command: "/usr/bin/python3",
		Args:    []string{"server.py"},
		Dir:     "/opt/mymcp",
		Env:     map[string]string{"FOO": "bar"},
	}
	cmd, args, dir, env := m.ResolveForPlatform()
	if cmd != m.Command || dir != m.Dir {
		t.Fatalf("expected base fields unchanged, got cmd=%q dir=%q", cmd, dir)
	}
	if !reflect.DeepEqual(args, m.Args) {
		t.Fatalf("expected base Args unchanged, got %v", args)
	}
	if !reflect.DeepEqual(env, m.Env) {
		t.Fatalf("expected base Env unchanged, got %v", env)
	}
}

// TestResolveForPlatform_NoMatchingOSKey_FallsBackToBase covers a
// PlatformOverrides map that exists but has no entry for the current
// runtime.GOOS -- must still fall back to the base fields, not zero them out.
func TestResolveForPlatform_NoMatchingOSKey_FallsBackToBase(t *testing.T) {
	m := &MCPDef{
		Command: "/usr/bin/python3",
		Args:    []string{"server.py"},
		PlatformOverrides: map[string]MCPPlatformOverride{
			"some-other-os-that-will-never-match": {Command: "/should/not/be/used"},
		},
	}
	cmd, _, _, _ := m.ResolveForPlatform()
	if cmd != m.Command {
		t.Fatalf("expected fallback to base Command, got %q", cmd)
	}
}

// TestResolveForPlatform_MatchingOSKey_Overrides is the actual feature: a
// PlatformOverrides entry for the CURRENT OS must take effect. Uses
// runtime.GOOS directly so this test is meaningful on whichever OS the CI
// actually runs on, mirroring the leann-mcp.json fix this test protects.
func TestResolveForPlatform_MatchingOSKey_Overrides(t *testing.T) {
	m := &MCPDef{
		Command: "/root/base/python",
		Args:    []string{"/root/base/server.py"},
		Dir:     "/root/base",
		Env:     map[string]string{"BASE": "1"},
		PlatformOverrides: map[string]MCPPlatformOverride{
			runtime.GOOS: {
				Command: "C:/override/python.exe",
				Args:    []string{"C:/override/server.py"},
				Dir:     "C:/override",
				Env:     map[string]string{"OVERRIDE": "1"},
			},
		},
	}
	cmd, args, dir, env := m.ResolveForPlatform()
	if cmd != "C:/override/python.exe" {
		t.Fatalf("expected overridden Command, got %q", cmd)
	}
	if !reflect.DeepEqual(args, []string{"C:/override/server.py"}) {
		t.Fatalf("expected overridden Args, got %v", args)
	}
	if dir != "C:/override" {
		t.Fatalf("expected overridden Dir, got %q", dir)
	}
	if !reflect.DeepEqual(env, map[string]string{"OVERRIDE": "1"}) {
		t.Fatalf("expected overridden Env, got %v", env)
	}
}

// TestResolveForPlatform_PartialOverride_OnlyOverridesSetFields verifies
// that an override entry setting only some fields (e.g. just Command,
// leaving Args/Dir/Env zero-valued) doesn't blank out the base fields for
// the ones it didn't set -- each field falls back independently.
func TestResolveForPlatform_PartialOverride_OnlyOverridesSetFields(t *testing.T) {
	m := &MCPDef{
		Command: "/root/base/python",
		Args:    []string{"/root/base/server.py"},
		Dir:     "/root/base",
		PlatformOverrides: map[string]MCPPlatformOverride{
			runtime.GOOS: {Command: "C:/override/python.exe"}, // Args/Dir/Env left zero
		},
	}
	cmd, args, dir, _ := m.ResolveForPlatform()
	if cmd != "C:/override/python.exe" {
		t.Fatalf("expected overridden Command, got %q", cmd)
	}
	if !reflect.DeepEqual(args, m.Args) {
		t.Fatalf("expected base Args preserved when override doesn't set Args, got %v", args)
	}
	if dir != m.Dir {
		t.Fatalf("expected base Dir preserved when override doesn't set Dir, got %q", dir)
	}
}
