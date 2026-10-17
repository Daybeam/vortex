package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestIsStickyResult_PositiveMatches verifies that tool names containing
// sticky patterns (auth, login, tokens) are correctly detected.
// Mutation target: flipping the return true → test fails.
func TestIsStickyResult_PositiveMatches(t *testing.T) {
	cases := []string{
		"login", "user_login", "login_user",
		"auth", "authenticate", "auth_check",
		"get_token", "get_session", "get_user",
		"signin", "connect", "get_api_key", "get_credentials",
	}
	for _, name := range cases {
		if !isStickyResult(name) {
			t.Errorf("isStickyResult(%q) = false, want true", name)
		}
	}
}

// TestIsStickyResult_NegativeMatches verifies that ordinary tool names
// are NOT classified as sticky.
// Mutation target: flipping the return false → test fails.
func TestIsStickyResult_NegativeMatches(t *testing.T) {
	cases := []string{
		"read_file", "write_file", "list_dir", "execute_command",
		"search", "create_task", "update_config", "delete_record",
		"capture_screenshot", "run_test", "",
	}
	for _, name := range cases {
		if isStickyResult(name) {
			t.Errorf("isStickyResult(%q) = true, want false", name)
		}
	}
}

// TestIsStickyResult_CaseInsensitive verifies that the detector is
// case-insensitive (LOGIN, Auth, GET_TOKEN all match).
// Mutation target: removing strings.ToLower → test fails.
func TestIsStickyResult_CaseInsensitive(t *testing.T) {
	cases := []string{"LOGIN", "Auth", "GET_TOKEN", "SignIn", "CONNECT"}
	for _, name := range cases {
		if !isStickyResult(name) {
			t.Errorf("isStickyResult(%q) = false, want true (case-insensitive)", name)
		}
	}
}

// TestPrependSticky_EmptyContext verifies that an empty stickyContext
// returns the original blocks unchanged (no spurious prepend).
// Mutation target: removing the `if stickyContext == ""` guard → test fails
// because an extra block would be prepended.
func TestPrependSticky_EmptyContext(t *testing.T) {
	original := []schemas.ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "text", Text: "world"},
	}
	result := prependSticky(original, "")
	if len(result) != len(original) {
		t.Fatalf("len(result) = %d, want %d (no prepend for empty context)", len(result), len(original))
	}
	for i := range original {
		if result[i].Text != original[i].Text {
			t.Errorf("result[%d].Text = %q, want %q", i, result[i].Text, original[i].Text)
		}
	}
}

// TestPrependSticky_NonEmptyContext verifies that a non-empty stickyContext
// prepends exactly one "## Persistent Context" block at position 0.
// Mutation target: changing the header text or skipping the prepend → test fails.
func TestPrependSticky_NonEmptyContext(t *testing.T) {
	original := []schemas.ContentBlock{
		{Type: "text", Text: "user message"},
	}
	sticky := "token=abc123\nuser_id=42"
	result := prependSticky(original, sticky)

	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2 (1 sticky + 1 original)", len(result))
	}
	if result[0].Type != "text" {
		t.Errorf("result[0].Type = %q, want \"text\"", result[0].Type)
	}
	if !strings.HasPrefix(result[0].Text, "## Persistent Context\n") {
		t.Errorf("result[0].Text = %q, want prefix \"## Persistent Context\\n\"", result[0].Text)
	}
	if !strings.Contains(result[0].Text, sticky) {
		t.Errorf("result[0].Text = %q, want to contain sticky content %q", result[0].Text, sticky)
	}
}

// TestPrependSticky_PreservesOriginalBlocks verifies that the original
// conversation blocks appear after the sticky block, unmodified.
// Mutation target: dropping the `blocks...` spread → test fails.
func TestPrependSticky_PreservesOriginalBlocks(t *testing.T) {
	original := []schemas.ContentBlock{
		{Type: "text", Text: "block-A"},
		{Type: "text", Text: "block-B"},
		{Type: "text", Text: "block-C"},
	}
	result := prependSticky(original, "session=xyz")

	if len(result) != 4 {
		t.Fatalf("len(result) = %d, want 4 (1 sticky + 3 original)", len(result))
	}
	for i, want := range original {
		got := result[i+1]
		if got.Text != want.Text {
			t.Errorf("result[%d].Text = %q, want %q", i+1, got.Text, want.Text)
		}
	}
}

// TestPrependSticky_NilBlocks verifies that prependSticky handles nil
// blocks gracefully (returns a slice with just the sticky block).
func TestPrependSticky_NilBlocks(t *testing.T) {
	result := prependSticky(nil, "token=abc")
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1 (sticky only)", len(result))
	}
	if !strings.HasPrefix(result[0].Text, "## Persistent Context\n") {
		t.Errorf("result[0].Text = %q, want persistent context header", result[0].Text)
	}
}

// TestStickyAccumulation_Dedup verifies that duplicate sticky results are
// not re-added to stickyContext.
// Mutation target: changing strings.Contains to always-false or removing
// the dedup check → test fails because duplicates would be appended.
func TestStickyAccumulation_Dedup(t *testing.T) {
	// Simulate two turns producing the same sticky result.
	stickyResultsTurn1 := []string{"token=abc123\nuser=alice"}
	stickyResultsTurn2 := []string{"token=abc123\nuser=alice"} // identical

	stickyContext := accumulateSticky("", stickyResultsTurn1)
	stickyContext = accumulateSticky(stickyContext, stickyResultsTurn2)

	// The sticky result should appear exactly once.
	count := strings.Count(stickyContext, "token=abc123")
	if count != 1 {
		t.Errorf("sticky result appears %d times, want 1 (dedup)", count)
	}
}

// TestStickyAccumulation_MultipleDistinct verifies that multiple
// different sticky results are all accumulated.
// Mutation target: breaking the append → test fails.
func TestStickyAccumulation_MultipleDistinct(t *testing.T) {
	results := []string{
		"token=abc123",
		"session=xyz789",
		"api_key=sk-abc",
	}

	stickyContext := accumulateSticky("", results)

	for _, want := range results {
		if !strings.Contains(stickyContext, want) {
			t.Errorf("stickyContext = %q, missing %q", stickyContext, want)
		}
	}
}

// TestStickyAccumulation_EmptySkipped verifies that empty sticky
// results are skipped (not appended as bare "\n\n").
// Mutation target: removing the `sr != ""` guard → test fails.
func TestStickyAccumulation_EmptySkipped(t *testing.T) {
	results := []string{"", "token=abc", ""}

	stickyContext := accumulateSticky("", results)

	// Only "token=abc" should be present.
	if !strings.Contains(stickyContext, "token=abc") {
		t.Errorf("stickyContext = %q, missing \"token=abc\"", stickyContext)
	}
	// Should not start with bare newlines from empty results.
	if strings.HasPrefix(stickyContext, "\n\n") {
		t.Errorf("stickyContext = %q, should not start with bare newlines", stickyContext)
	}
}
