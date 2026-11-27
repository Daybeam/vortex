package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Register Q3 regression: root acp.dialect was discarded before reaching the
// transport; omitted and invalid values must retain auto behavior.
func TestACP_DialectConfigLoad(t *testing.T) {
	for _, tt := range []struct{ raw, want string }{
		{`{"providers":{"default":{"provider":"openai","model":"m"}},"acp":{"dialect":"standard"}}`, "standard"},
		{`{"providers":{"default":{"provider":"openai","model":"m"}}}`, "auto"},
		{`{"providers":{"default":{"provider":"openai","model":"m"}},"acp":{"dialect":"invalid"}}`, "auto"},
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(tt.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		reg, err := NewRegistry(path)
		if err != nil {
			t.Fatal(err)
		}
		if reg.ACP.Dialect != tt.want {
			t.Errorf("dialect = %q, want %q", reg.ACP.Dialect, tt.want)
		}
	}
}
