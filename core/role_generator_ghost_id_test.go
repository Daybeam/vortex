package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestSlugifyID covers the ID-derivation helper added alongside the
// empty-roleID ghost-role/ghost-skill fix (see role_generator.go's FIX
// comment on GenerateRoleObjects for the full incident this closes).
func TestSlugifyID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "Security Audit Specialist", "security_audit_specialist"},
		{"punctuation collapses to one underscore", "API/Key   Manager!!!", "api_key_manager"},
		{"leading and trailing junk trimmed", "  --Foo Bar--  ", "foo_bar"},
		{"empty input yields empty output", "", ""},
		{"pure punctuation yields empty output", "!!!---***", ""},
		{"mixed case and digits preserved", "GPT4 Reviewer v2", "gpt4_reviewer_v2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slugifyID(tc.in)
			if got != tc.want {
				t.Errorf("slugifyID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("long names are truncated to 48 chars", func(t *testing.T) {
		long := "This Is A Very Long Role Name That Goes On And On And On And On"
		got := slugifyID(long)
		if len(got) > 48 {
			t.Errorf("slugifyID produced a %d-char ID, want <= 48: %q", len(got), got)
		}
	})
}

// TestGenerateRoleObjects_EmptyRoleID_FailsCleanlyNotWithGhostID is a direct
// regression test for the bug found in the 2026-08-10/11 addendum session: a
// real, LIVE-LOADED orphan skill (workspace/skills/skill_base_.json, id
// "skill_base_", description "Auto-generated base skill for " truncated) was
// found sitting in the registry. It was produced by GenerateRoleObjects being
// called with roleID == "" (a legitimate call shape -- see
// builtin_interceptors.go's ephemeral/dynamic generation branches, not a
// caller bug) and using that empty string as-is for both the role ID and the
// "skill_base_<roleID>" skill ID naming scheme.
//
// This mirrors TestGenerateRoleObjects_NilResourceLoader_ReturnsErrorNotPanic
// exactly (same nil-resourceLoader fast-fail shape, since a real LLM call
// isn't available in this test environment), but with roleID == "" instead
// of a real role name -- confirming the empty-roleID call shape is handled
// by the same early guard as any other call, without panicking or behaving
// differently before ever reaching the ID-derivation logic. Full live
// coverage of the ID-substitution logic itself (does an empty roleID really
// get replaced by a slugified name from the LLM's response) is deferred to
// live verification against a real provider, consistent with this
// codebase's existing precedent for LLM-calling code paths -- see
// TestSlugifyID above for the actual derivation-logic coverage.
func TestGenerateRoleObjects_EmptyRoleID_FailsCleanlyNotWithGhostID(t *testing.T) {
	reg := &config.Registry{
		DefaultProvider: "main",
		Providers: map[string]*config.ProviderConfig{
			"main": {
				Provider:  "openai",
				Model:     "gpt-4o",
				APIKeyEnv: "SOME_KEY_THAT_NEED_NOT_EXIST",
			},
		},
		RoleCookbookSource: "github:anthropics/anthropic-cookbook",
	}

	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	defer logger.Close()

	gen := NewRoleGenerator(reg, nil, logger) // deliberately nil resourceLoader

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GenerateRoleObjects panicked with roleID==\"\" instead of returning an error: %v", r)
		}
	}()

	role, skill, err := gen.GenerateRoleObjects(context.Background(), "task1", "step1", "", "do something useful")
	if err == nil {
		t.Fatal("expected an error (nil resourceLoader), got nil")
	}
	if role != nil {
		t.Errorf("expected a nil role on error, got %+v", role)
	}
	if skill != nil {
		t.Errorf("expected a nil skill on error, got %+v", skill)
	}
}
