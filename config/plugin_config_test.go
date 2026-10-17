package config

import (
	"encoding/json"
	"testing"
)

func TestPluginConfig_UnmarshalJSON_PlainString(t *testing.T) {
	// Backward compat: a plain string should unmarshal into PluginConfig{Script: s}
	data := []byte(`"plugins/sentiment.py"`)
	var pc PluginConfig
	if err := json.Unmarshal(data, &pc); err != nil {
		t.Fatalf("plain string unmarshal failed: %v", err)
	}
	if pc.Script != "plugins/sentiment.py" {
		t.Errorf("expected Script='plugins/sentiment.py', got %q", pc.Script)
	}
	if pc.SHA256 != "" {
		t.Errorf("expected empty SHA256 for plain string, got %q", pc.SHA256)
	}
}

func TestPluginConfig_UnmarshalJSON_FullObject(t *testing.T) {
	data := []byte(`{
		"script": "plugins/sentiment.py",
		"sha256": "abc123",
		"permissions": {
			"network": ["api.sentimes.cn"],
			"filesystem": ["read:workspace/", "write:outputs/"],
			"env_vars": ["SENTIMES_API_KEY"],
			"subprocess": false
		},
		"timeout_ms": 15000
	}`)
	var pc PluginConfig
	if err := json.Unmarshal(data, &pc); err != nil {
		t.Fatalf("full object unmarshal failed: %v", err)
	}
	if pc.Script != "plugins/sentiment.py" {
		t.Errorf("expected Script='plugins/sentiment.py', got %q", pc.Script)
	}
	if pc.SHA256 != "abc123" {
		t.Errorf("expected SHA256='abc123', got %q", pc.SHA256)
	}
	if pc.TimeoutMs != 15000 {
		t.Errorf("expected TimeoutMs=15000, got %d", pc.TimeoutMs)
	}
	if len(pc.Permissions.Network) != 1 || pc.Permissions.Network[0] != "api.sentimes.cn" {
		t.Errorf("expected Network=['api.sentimes.cn'], got %v", pc.Permissions.Network)
	}
	if len(pc.Permissions.EnvVars) != 1 || pc.Permissions.EnvVars[0] != "SENTIMES_API_KEY" {
		t.Errorf("expected EnvVars=['SENTIMES_API_KEY'], got %v", pc.Permissions.EnvVars)
	}
}

func TestPluginConfig_UnmarshalJSON_ObjectWithHashOnly(t *testing.T) {
	// Minimal object: just script + sha256, no permissions
	data := []byte(`{"script": "hooks/audit.lua", "sha256": "b2c3d4e5f6a7"}`)
	var pc PluginConfig
	if err := json.Unmarshal(data, &pc); err != nil {
		t.Fatalf("minimal object unmarshal failed: %v", err)
	}
	if pc.Script != "hooks/audit.lua" {
		t.Errorf("expected Script='hooks/audit.lua', got %q", pc.Script)
	}
	if pc.SHA256 != "b2c3d4e5f6a7" {
		t.Errorf("expected SHA256='b2c3d4e5f6a7', got %q", pc.SHA256)
	}
}

func TestExternalInterceptors_BackwardCompat_StringArray(t *testing.T) {
	// Old-style config: external_interceptors as []string
	data := []byte(`{"external_interceptors": ["plugins/legacy1.py", "plugins/legacy2.js"]}`)
	var sys struct {
		ExternalInterceptors []PluginConfig `json:"external_interceptors"`
	}
	if err := json.Unmarshal(data, &sys); err != nil {
		t.Fatalf("string array unmarshal failed: %v", err)
	}
	if len(sys.ExternalInterceptors) != 2 {
		t.Fatalf("expected 2 interceptors, got %d", len(sys.ExternalInterceptors))
	}
	if sys.ExternalInterceptors[0].Script != "plugins/legacy1.py" {
		t.Errorf("expected first script='plugins/legacy1.py', got %q", sys.ExternalInterceptors[0].Script)
	}
	if sys.ExternalInterceptors[1].Script != "plugins/legacy2.js" {
		t.Errorf("expected second script='plugins/legacy2.js', got %q", sys.ExternalInterceptors[1].Script)
	}
}

func TestExternalInterceptors_MixedStringAndObject(t *testing.T) {
	// Mixed: some as plain strings, some as full objects
	data := []byte(`{"external_interceptors": [
		"plugins/legacy.py",
		{"script": "plugins/new.py", "sha256": "abc123", "permissions": {"env_vars": ["API_KEY"]}}
	]}`)
	var sys struct {
		ExternalInterceptors []PluginConfig `json:"external_interceptors"`
	}
	if err := json.Unmarshal(data, &sys); err != nil {
		t.Fatalf("mixed array unmarshal failed: %v", err)
	}
	if len(sys.ExternalInterceptors) != 2 {
		t.Fatalf("expected 2 interceptors, got %d", len(sys.ExternalInterceptors))
	}
	// First is plain string
	if sys.ExternalInterceptors[0].Script != "plugins/legacy.py" || sys.ExternalInterceptors[0].SHA256 != "" {
		t.Errorf("first should be plain string: %+v", sys.ExternalInterceptors[0])
	}
	// Second is full object
	if sys.ExternalInterceptors[1].Script != "plugins/new.py" || sys.ExternalInterceptors[1].SHA256 != "abc123" {
		t.Errorf("second should be full object: %+v", sys.ExternalInterceptors[1])
	}
	if len(sys.ExternalInterceptors[1].Permissions.EnvVars) != 1 || sys.ExternalInterceptors[1].Permissions.EnvVars[0] != "API_KEY" {
		t.Errorf("second should have EnvVars=['API_KEY']: %+v", sys.ExternalInterceptors[1].Permissions)
	}
}

func TestHookConfig_SHA256AndPermissions(t *testing.T) {
	data := []byte(`{
		"point": "step_pre",
		"script": "hooks/audit.lua",
		"sha256": "b2c3d4e5f6a7",
		"permissions": {
			"filesystem": ["read:workspace/", "write:logs/"],
			"env_vars": [],
			"subprocess": false
		}
	}`)
	var h HookConfig
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatalf("hook config unmarshal failed: %v", err)
	}
	if h.Point != "step_pre" || h.Script != "hooks/audit.lua" {
		t.Errorf("unexpected point/script: %+v", h)
	}
	if h.SHA256 != "b2c3d4e5f6a7" {
		t.Errorf("expected SHA256='b2c3d4e5f6a7', got %q", h.SHA256)
	}
	if len(h.Permissions.Filesystem) != 2 {
		t.Errorf("expected 2 fs perms, got %d", len(h.Permissions.Filesystem))
	}
}
