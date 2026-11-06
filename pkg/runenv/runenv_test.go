package runenv

import (
	"os"
	"testing"
)

// Regression for §13.6 VORTEX_ROOT deployment seam: the env var must pin the
// data root and override the exe-dir/cwd heuristic. Before this fix, root was
// always derived from os.Executable() with no env override (multi-tenant
// deployments could not isolate data per process).

func TestResolveDataRoot_HonorsVortexRootEnv(t *testing.T) {
	t.Setenv("VORTEX_ROOT", "/explicit/root")
	got := ResolveDataRoot(func() string { return "/fallback" })
	if got != "/explicit/root" {
		t.Fatalf("ResolveDataRoot() = %q, want %q (VORTEX_ROOT must win)", got, "/explicit/root")
	}
}

func TestResolveDataRoot_FallsBackWhenEnvUnset(t *testing.T) {
	os.Unsetenv("VORTEX_ROOT")
	got := ResolveDataRoot(func() string { return "/fallback" })
	if got != "/fallback" {
		t.Fatalf("ResolveDataRoot() = %q, want %q (fallback when env unset)", got, "/fallback")
	}
}

func TestResolveDataRoot_FallsBackWhenEnvEmpty(t *testing.T) {
	t.Setenv("VORTEX_ROOT", "")
	got := ResolveDataRoot(func() string { return "/fallback" })
	if got != "/fallback" {
		t.Fatalf("ResolveDataRoot() = %q, want %q (empty env must not pin root)", got, "/fallback")
	}
}
