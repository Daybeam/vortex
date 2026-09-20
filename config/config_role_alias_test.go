package config

import (
	"encoding/json"
	"testing"
)

// Regression coverage for the 2026-08-06/08-08 uncommitted-then-rescued
// MCPDef.Dir/Env + Role/GroupMember ID-alias work.

func TestRole_UnmarshalJSON_IDAliases(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"id field", `{"id":"system_coder"}`, "system_coder"},
		{"role_id alias", `{"role_id":"system_coder"}`, "system_coder"},
		{"role alias", `{"role":"system_coder"}`, "system_coder"},
		{"id takes precedence over aliases", `{"id":"real","role_id":"ignored","role":"ignored2"}`, "real"},
		{"role_id takes precedence over role", `{"role_id":"first","role":"second"}`, "first"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r Role
			if err := json.Unmarshal([]byte(c.json), &r); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if r.ID != c.want {
				t.Errorf("got ID=%q, want %q", r.ID, c.want)
			}
		})
	}
}

func TestRole_UnmarshalJSON_OtherFieldsPreserved(t *testing.T) {
	var r Role
	if err := json.Unmarshal([]byte(`{"role":"system_coder","base_capability":"code"}`), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.ID != "system_coder" {
		t.Errorf("got ID=%q, want system_coder", r.ID)
	}
	if r.BaseCapability != "code" {
		t.Errorf("got BaseCapability=%q, want code (alias unmarshal must not drop other fields)", r.BaseCapability)
	}
}

func TestGroupMember_UnmarshalJSON_RoleAlias(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"role_id field", `{"role_id":"system_coder"}`, "system_coder"},
		{"role alias", `{"role":"system_coder"}`, "system_coder"},
		{"role_id takes precedence over role", `{"role_id":"first","role":"second"}`, "first"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m GroupMember
			if err := json.Unmarshal([]byte(c.json), &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if m.RoleID != c.want {
				t.Errorf("got RoleID=%q, want %q", m.RoleID, c.want)
			}
		})
	}
}

func TestGroupMember_UnmarshalJSON_OtherFieldsPreserved(t *testing.T) {
	var m GroupMember
	if err := json.Unmarshal([]byte(`{"role":"system_coder","optional":true}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.RoleID != "system_coder" {
		t.Errorf("got RoleID=%q, want system_coder", m.RoleID)
	}
	if !m.Optional {
		t.Errorf("Optional field was dropped by alias unmarshal")
	}
}

func TestMCPDef_DirEnv_JSONRoundTrip(t *testing.T) {
	src := &MCPDef{
		ID:      "melodie",
		Command: "python",
		Dir:     "D:/some/project/root",
		Env:     map[string]string{"PYTHONPATH": "D:/some/project/root"},
	}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out MCPDef
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Dir != src.Dir {
		t.Errorf("Dir round-trip: got %q, want %q", out.Dir, src.Dir)
	}
	if out.Env["PYTHONPATH"] != "D:/some/project/root" {
		t.Errorf("Env round-trip failed, got %v", out.Env)
	}
}

func TestMCPDef_DirEnv_OmittedWhenEmpty(t *testing.T) {
	src := &MCPDef{ID: "exa", URL: "https://mcp.exa.ai/mcp"}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if _, ok := raw["dir"]; ok {
		t.Errorf("dir should be omitted when empty (omitempty), got present in %v", raw)
	}
	if _, ok := raw["env"]; ok {
		t.Errorf("env should be omitted when empty (omitempty), got present in %v", raw)
	}
}
