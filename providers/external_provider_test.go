package providers

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/env"
)

// TestNewExternalScriptProvider_BuiltinExtensionsUnaffected verifies the
// pre-existing .py/.js behavior is unchanged by the 2026-07-14 extensibility
// fix (backward compatibility check).
func TestNewExternalScriptProvider_BuiltinExtensionsUnaffected(t *testing.T) {
	p, err := NewExternalScriptProvider(&config.ProviderConfig{}, "script.py", config.ExternalRuntimes{})
	if err != nil {
		t.Fatalf("unexpected error for .py: %v", err)
	}
	expectedExe := env.GetPythonCmd()
	if p.runtime != "python" || p.exePath != expectedExe || len(p.extraArgs) != 0 {
		t.Fatalf("unexpected fields for .py: runtime=%q exePath=%q extraArgs=%v", p.runtime, p.exePath, p.extraArgs)
	}

	p2, err := NewExternalScriptProvider(&config.ProviderConfig{}, "script.js", config.ExternalRuntimes{NodePath: "/custom/node"})
	if err != nil {
		t.Fatalf("unexpected error for .js: %v", err)
	}
	if p2.runtime != "node" || p2.exePath != "/custom/node" {
		t.Fatalf("unexpected fields for .js with custom NodePath: runtime=%q exePath=%q", p2.runtime, p2.exePath)
	}
}

// TestNewExternalScriptProvider_UnregisteredExtensionFails verifies an
// extension with no config.ExternalRuntimes.Runtimes entry still fails
// clearly, instead of silently picking some default.
func TestNewExternalScriptProvider_UnregisteredExtensionFails(t *testing.T) {
	_, err := NewExternalScriptProvider(&config.ProviderConfig{}, "script.rb", config.ExternalRuntimes{})
	if err == nil {
		t.Fatal("expected error for unregistered .rb extension, got nil")
	}
}

// TestNewExternalScriptProvider_DeveloperRegisteredExtension verifies the
// new config.ExternalRuntimes.Runtimes escape hatch: a developer can register
// any extension -> [command, extra_args...] without touching Go source.
func TestNewExternalScriptProvider_DeveloperRegisteredExtension(t *testing.T) {
	runtimes := config.ExternalRuntimes{
		Runtimes: map[string][]string{
			".rb": {"ruby"},
			".ts": {"deno", "run"},
		},
	}

	p, err := NewExternalScriptProvider(&config.ProviderConfig{}, "script.rb", runtimes)
	if err != nil {
		t.Fatalf("unexpected error for registered .rb: %v", err)
	}
	if p.runtime != "ruby" || p.exePath != "ruby" || len(p.extraArgs) != 0 {
		t.Fatalf("unexpected fields for .rb: runtime=%q exePath=%q extraArgs=%v", p.runtime, p.exePath, p.extraArgs)
	}

	p2, err := NewExternalScriptProvider(&config.ProviderConfig{}, "script.ts", runtimes)
	if err != nil {
		t.Fatalf("unexpected error for registered .ts: %v", err)
	}
	if p2.runtime != "deno" || p2.exePath != "deno" || len(p2.extraArgs) != 1 || p2.extraArgs[0] != "run" {
		t.Fatalf("unexpected fields for .ts: runtime=%q exePath=%q extraArgs=%v", p2.runtime, p2.exePath, p2.extraArgs)
	}
}

// TestNewExternalScriptProvider_EmptyRuntimesEntryFails verifies a config
// entry with an empty command list is treated as not-registered, not as a
// valid-but-empty command.
func TestNewExternalScriptProvider_EmptyRuntimesEntryFails(t *testing.T) {
	runtimes := config.ExternalRuntimes{
		Runtimes: map[string][]string{
			".rb": {},
		},
	}
	_, err := NewExternalScriptProvider(&config.ProviderConfig{}, "script.rb", runtimes)
	if err == nil {
		t.Fatal("expected error for .rb with empty command list, got nil")
	}
}
