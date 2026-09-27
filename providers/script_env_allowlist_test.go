package providers

import (
	"os"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// TestSM5_EnvBridge_RejectsSensitiveVars is a regression test for audit S-M5:
// the Lua env() bridge in ScriptProvider must reject environment variable names
// containing KEY, SECRET, TOKEN, PASSWORD, or CREDENTIAL to prevent secret
// exfiltration via Lua scripts.
//
// Before the fix, env() returned os.Getenv(name) for any name, allowing scripts
// to exfiltrate secrets like env("VORTEX_ADMIN_KEY").
// After the fix, names containing sensitive keywords return "".
//
// Reproduction: call env() with a sensitive var name and verify it returns "".
func TestSM5_EnvBridge_RejectsSensitiveVars(t *testing.T) {
	sp := &ScriptProvider{}

	vm := lua.NewState()
	defer vm.Close()
	lua.OpenBase(vm)
	lua.OpenString(vm)
	sp.registerBridge(vm)

	// Set a real env var so we can confirm the bridge WOULD return it without the fix.
	os.Setenv("MY_SECRET_KEY", "super-secret-value")
	os.Setenv("API_TOKEN", "token-123")
	os.Setenv("DB_PASSWORD", "pw-456")
	os.Setenv("MY_CREDENTIAL", "cred-789")
	defer os.Unsetenv("MY_SECRET_KEY")
	defer os.Unsetenv("API_TOKEN")
	defer os.Unsetenv("DB_PASSWORD")
	defer os.Unsetenv("MY_CREDENTIAL")

	sensitiveNames := []string{
		"MY_SECRET_KEY",
		"API_TOKEN",
		"DB_PASSWORD",
		"MY_CREDENTIAL",
		"VORTEX_ADMIN_KEY",
		"SECRET",
		"TOKEN",
		"PASSWORD",
		"KEY",
		"CREDENTIAL",
		"my_key",
		"api_secret",
		"auth_token",
		"user_password",
		"db_credential",
	}

	for _, name := range sensitiveNames {
		err := vm.DoString(`return env("` + name + `")`)
		if err != nil {
			t.Errorf("sensitive var %q: Lua error: %v", name, err)
			continue
		}
		result := vm.Get(-1)
		vm.Pop(1)
		if result.String() != "" {
			t.Errorf("sensitive var %q: expected empty string, got %q (secret leaked!)", name, result.String())
		}
	}
}

// TestSM5_EnvBridge_AllowsNonSensitiveVars verifies that the S-M5 fix does not
// over-reject legitimate non-sensitive environment variables.
func TestSM5_EnvBridge_AllowsNonSensitiveVars(t *testing.T) {
	sp := &ScriptProvider{}

	vm := lua.NewState()
	defer vm.Close()
	lua.OpenBase(vm)
	lua.OpenString(vm)
	sp.registerBridge(vm)

	os.Setenv("HOME", "/tmp/test-home")
	os.Setenv("PATH", "/usr/bin:/bin")
	os.Setenv("LANG", "en_US.UTF-8")
	defer os.Unsetenv("HOME")
	defer os.Unsetenv("PATH")
	defer os.Unsetenv("LANG")

	safeNames := []string{"HOME", "PATH", "LANG", "NONEXISTENT_VAR"}

	for _, name := range safeNames {
		err := vm.DoString(`return env("` + name + `")`)
		if err != nil {
			t.Errorf("safe var %q: Lua error: %v", name, err)
			continue
		}
		result := vm.Get(-1)
		vm.Pop(1)
		expected := os.Getenv(name)
		if result.String() != expected {
			t.Errorf("safe var %q: expected %q, got %q", name, expected, result.String())
		}
	}
}
