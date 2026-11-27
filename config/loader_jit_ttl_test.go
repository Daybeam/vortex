package config

import (
	"os"
	"path/filepath"
	"testing"
)

// N3 regression: zero means "use the reflection pipeline's built-in TTL", not
// a one-hour override. Both bootstrap and normal file loading must preserve it.
func TestLoad_DefaultJITTTLZeroSemantics(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "missing config"},
		{name: "explicit zero", content: `{"system":{"default_jit_ttl":0}}`},
		{name: "omitted", content: `{"system":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			reg, err := NewRegistry(path)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			if reg.System.DefaultJITTTL != 0 {
				t.Fatalf("default_jit_ttl = %d, want zero semantics", reg.System.DefaultJITTTL)
			}
		})
	}
}

func TestLoad_DefaultJITTTLPositiveOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"system":{"default_jit_ttl":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := NewRegistry(path)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg.System.DefaultJITTTL != 7 {
		t.Fatalf("default_jit_ttl = %d, want 7", reg.System.DefaultJITTTL)
	}
}
