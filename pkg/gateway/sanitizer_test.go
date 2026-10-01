package gateway

import (
	"strings"
	"testing"
)

func TestSanitizer_MaskReplacesSecrets(t *testing.T) {
	s := NewSanitizerProxy()
	defer s.Clear("task1")

	input := "Use the key sk-abcdefghijklmnopqrstuvwxyz123456 to connect."
	masked := s.Mask(input, "task1")

	if strings.Contains(masked, "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatal("masked output still contains the raw API key")
	}
	if !strings.Contains(masked, "[REDACTED_1]") {
		t.Fatalf("expected [REDACTED_1] token in output, got: %s", masked)
	}
}

func TestSanitizer_BidirectionalConsistency(t *testing.T) {
	s := NewSanitizerProxy()
	defer s.Clear("task1")

	tests := []string{
		"sk-abcdefghijklmnopqrstuvwxyz123456",
		"password=mysecretpassword123",
		"api_key: AIzaSyABCDEFGHIJKLMNOabcdefghijklmnopqrstuvwxyz123456",
		"no secrets here",
	}
	for _, original := range tests {
		masked := s.Mask(original, "task1")
		restored := s.Unmask(masked, "task1")
		if restored != original {
			t.Errorf("bidirectional failure:\n  original: %q\n  masked:   %q\n  restored: %q", original, masked, restored)
		}
	}
}

func TestSanitizer_SessionIsolation(t *testing.T) {
	s := NewSanitizerProxy()
	defer s.Clear("task1")
	defer s.Clear("task2")

	// Mask different secrets in different sessions
	secret1 := "sk-abcdefghijklmnopqrstuvwxyz123456"
	secret2 := "sk-zyxwvutsrqponmlkjihgfedcba654321"

	masked1 := s.Mask(secret1, "task1")
	masked2 := s.Mask(secret2, "task2")

	// task1 should not be able to unmask task2's secret
	restoreWithWrongSession := s.Unmask(masked2, "task1")
	if restoreWithWrongSession == secret2 {
		t.Fatal("session isolation violated: task1 vault restored task2's secret")
	}
	// task1's vault should correctly restore its own secret
	restoreCorrect := s.Unmask(masked1, "task1")
	if restoreCorrect != secret1 {
		t.Fatalf("task1 failed to restore its own secret: got %q", restoreCorrect)
	}
	// task2's vault should correctly restore its own secret
	restoreCorrect2 := s.Unmask(masked2, "task2")
	if restoreCorrect2 != secret2 {
		t.Fatalf("task2 failed to restore its own secret: got %q", restoreCorrect2)
	}
}

func TestSanitizer_DeterministicTokenWithinSession(t *testing.T) {
	s := NewSanitizerProxy()
	defer s.Clear("task1")

	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	input1 := "First use: " + secret
	input2 := "Second use: " + secret

	masked1 := s.Mask(input1, "task1")
	masked2 := s.Mask(input2, "task1")

	// Same secret should get the same token in both outputs.
	if masked1 != "First use: [REDACTED_1]" {
		t.Fatalf("expected 'First use: [REDACTED_1]', got %q", masked1)
	}
	if masked2 != "Second use: [REDACTED_1]" {
		t.Fatalf("expected 'Second use: [REDACTED_1]', got %q", masked2)
	}
}

func TestSanitizer_MultipleSecretsIncrementTokens(t *testing.T) {
	s := NewSanitizerProxy()
	defer s.Clear("task1")

	input := "key1=sk-abcdefghijklmnopqrstuvwxyz123456 and key2=sk-zyxwvutsrqponmlkjihgfedcba654321"
	masked := s.Mask(input, "task1")

	if !strings.Contains(masked, "[REDACTED_1]") {
		t.Fatalf("expected [REDACTED_1] in output, got: %s", masked)
	}
	if !strings.Contains(masked, "[REDACTED_2]") {
		t.Fatalf("expected [REDACTED_2] in output, got: %s", masked)
	}

	// Unmask should restore both
	restored := s.Unmask(masked, "task1")
	if restored != input {
		t.Fatalf("bidirectional failure for multiple secrets:\n  original: %q\n  restored: %q", input, restored)
	}
}

func TestSanitizer_ClearRemovesVault(t *testing.T) {
	s := NewSanitizerProxy()

	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	masked := s.Mask(secret, "task1")

	// Clear the vault
	s.Clear("task1")

	// Unmask after clear should NOT restore (vault gone)
	restored := s.Unmask(masked, "task1")
	if restored == secret {
		t.Fatal("Clear did not remove vault — secret was still restored after Clear")
	}
	// Should return the masked input unchanged
	if restored != masked {
		t.Fatalf("expected masked input unchanged after Clear, got %q", restored)
	}
}

func TestSanitizer_NoSecretsPassThrough(t *testing.T) {
	s := NewSanitizerProxy()
	defer s.Clear("task1")

	input := "This is a normal prompt with no secrets."
	masked := s.Mask(input, "task1")

	if masked != input {
		t.Fatalf("input without secrets should pass through unchanged, got: %s", masked)
	}
}

func TestSanitizer_UnmaskNoVaultIsFailSafe(t *testing.T) {
	s := NewSanitizerProxy()

	// Unmask with a non-existent taskID should return input unchanged
	// (fail-safe: no data fabrication)
	input := "some [REDACTED_1] tokens"
	restored := s.Unmask(input, "nonexistent")
	if restored != input {
		t.Fatalf("Unmask with no vault should return input unchanged, got: %s", restored)
	}
}
