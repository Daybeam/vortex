package observability

import "testing"

// TestRedactMap_KeepsTokenCounts is a regression guard for silent data loss.
//
// RedactMap used to decide "is this a secret?" by substring-matching the key
// against a list that included "token". That is true of access_token and also
// of prompt_tokens, so every provider call's token accounting was written to
// the logs as "****". The numbers existed and were correct in memory; only the
// log copy was destroyed, which is the worst kind of observability bug because
// the field looks populated.
func TestRedactMap_KeepsTokenCounts(t *testing.T) {
	in := map[string]any{
		"prompt_tokens":     25463,
		"completion_tokens": 8063,
		"total_tokens":      33526,
		"cache_read_tokens": 1200,
		"token_count":       42,
	}
	out := RedactMap(in)
	for k, want := range in {
		if got := out[k]; got != want {
			t.Errorf("%s: got %v, want %v (counting fields must never be masked)", k, got, want)
		}
	}
}

// TestRedactMap_StillMasksCredentials is the non-vacuity guard: narrowing the
// match must not turn redaction off. These are the shapes that carry real
// secrets and must stay masked.
func TestRedactMap_StillMasksCredentials(t *testing.T) {
	cases := []string{
		"key", "api_key", "apikey", "access_key", "secret",
		"secret_key", "client_secret", "password", "passwd",
		"authorization", "token", "access_token", "refresh_token",
		"auth_token", "credential", "BEARER",
	}
	for _, k := range cases {
		out := RedactMap(map[string]any{k: "supersecretvalue"})
		if out[k] != "supe****" {
			t.Errorf("%s should be masked, got %v", k, out[k])
		}
	}
}

// TestRedactMap_ShortSecretIsFullyMasked covers values too short to reveal a
// 4-char prefix without effectively leaking them.
func TestRedactMap_ShortSecretIsFullyMasked(t *testing.T) {
	out := RedactMap(map[string]any{"token": "abc"})
	if out["token"] != "****" {
		t.Errorf("short secret should be fully masked, got %v", out["token"])
	}
}

// TestRedactMap_RecursesIntoNestedMaps keeps nested detail blocks covered.
func TestRedactMap_RecursesIntoNestedMaps(t *testing.T) {
	out := RedactMap(map[string]any{
		"usage": map[string]any{"prompt_tokens": 100, "api_key": "sk-abcdefghijk"},
	})
	nested, ok := out["usage"].(map[string]any)
	if !ok {
		t.Fatal("nested map was not preserved")
	}
	if nested["prompt_tokens"] != 100 {
		t.Errorf("nested token count masked: %v", nested["prompt_tokens"])
	}
	if nested["api_key"] != "sk-a****" {
		t.Errorf("nested api_key not masked: %v", nested["api_key"])
	}
}

// TestIsSensitiveKey pins the whole-key semantics: a key that merely CONTAINS
// a sensitive word is not a credential.
func TestIsSensitiveKey(t *testing.T) {
	notSensitive := []string{"prompt_tokens", "completion_tokens", "token_budget", "max_tokens", "keyword", "monkey"}
	for _, k := range notSensitive {
		if isSensitiveKey(k) {
			t.Errorf("%q must not be treated as sensitive", k)
		}
	}
	sensitive := []string{"token", "api_key", "password"}
	for _, k := range sensitive {
		if !isSensitiveKey(k) {
			t.Errorf("%q must be treated as sensitive", k)
		}
	}
}
