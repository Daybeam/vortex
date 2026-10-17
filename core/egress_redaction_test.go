package core

import (
	"strings"
	"testing"
)

// ─── Egress Redaction Unit Tests ──────────────────────────────────────────

func TestRedactEgress_OpenAIKey(t *testing.T) {
	input := `The key is sk-abcdefghijklmnopqrstuvwxyz123456 and it works.`
	got := RedactEgress(input)
	if strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Error("OpenAI key not redacted")
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Error("redacted marker missing")
	}
}

func TestRedactEgress_AWSKey(t *testing.T) {
	input := `AWS key: AKIAIOSFODNN7EXAMPLE`
	got := RedactEgress(input)
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Error("AWS key not redacted")
	}
}

func TestRedactEgress_PrivateKey(t *testing.T) {
	input := `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA1234567890
-----END RSA PRIVATE KEY-----`
	got := RedactEgress(input)
	if strings.Contains(got, "MIIEpAIBAAKCAQEA1234567890") {
		t.Error("private key content not redacted")
	}
	if !strings.Contains(got, "[PRIVATE KEY REDACTED]") {
		t.Error("private key marker missing")
	}
}

func TestRedactEgress_BearerToken(t *testing.T) {
	input := `Authorization: Bearer abcdefghijklmnopqrstuvwxyz1234567890`
	got := RedactEgress(input)
	if strings.Contains(got, "abcdefghijklmnopqrstuvwxyz1234567890") {
		t.Error("bearer token not redacted")
	}
}

func TestRedactEgress_JWT(t *testing.T) {
	input := `token: eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryPpJuzg7p1vX9uX9wX9wX9wX9wX9wX9wX9w`
	got := RedactEgress(input)
	if strings.Contains(got, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9") {
		t.Error("JWT not redacted")
	}
}

func TestRedactEgress_Email(t *testing.T) {
	input := `Contact: user@example.com for details.`
	got := RedactEgress(input)
	if strings.Contains(got, "user@example.com") {
		t.Error("email not redacted")
	}
	if !strings.Contains(got, "[EMAIL REDACTED]") {
		t.Error("email marker missing")
	}
}

func TestRedactEgress_PhoneCN(t *testing.T) {
	input := `Phone: 13812345678`
	got := RedactEgress(input)
	if strings.Contains(got, "13812345678") {
		t.Error("phone number not redacted")
	}
}

func TestRedactEgress_NoSecrets(t *testing.T) {
	input := `The task completed successfully. Output: 42 files created.`
	got := RedactEgress(input)
	if got != input {
		t.Errorf("text without secrets was modified: %q", got)
	}
}

func TestRedactEgress_MultipleSecrets(t *testing.T) {
	input := `key=sk-abcdefghijklmnopqrstuvwxyz123456 email=admin@test.com phone=13912345678`
	got := RedactEgress(input)
	if strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Error("OpenAI key not redacted in multi-secret input")
	}
	if strings.Contains(got, "admin@test.com") {
		t.Error("email not redacted in multi-secret input")
	}
	if strings.Contains(got, "13912345678") {
		t.Error("phone not redacted in multi-secret input")
	}
}

func TestRedactEgressWithOwner_NoCrossTenant(t *testing.T) {
	text := `owner: tenant-a completed the task successfully.`
	got := RedactEgressWithOwner(text, "tenant-a")
	// Same owner → no redaction of owner ID.
	if !strings.Contains(got, "tenant-a") {
		t.Error("same owner ID was incorrectly redacted")
	}
}

func TestRedactEgressWithOwner_CrossTenantDetected(t *testing.T) {
	text := `owner: tenant-b accessed the resource.`
	got := RedactEgressWithOwner(text, "tenant-a")
	// Different owner → should be redacted.
	if strings.Contains(got, "tenant-b") {
		t.Error("cross-tenant owner ID not redacted")
	}
	if !strings.Contains(got, "[OTHER_TENANT:REDACTED]") {
		t.Error("cross-tenant marker missing")
	}
}

func TestRedactEgressWithOwner_EmptyOwner(t *testing.T) {
	text := `owner: tenant-b data here.`
	got := RedactEgressWithOwner(text, "")
	// No current owner → skip cross-tenant check, only basic redaction.
	if !strings.Contains(got, "tenant-b") {
		t.Error("with empty owner, cross-tenant check should be skipped")
	}
}

func TestRedactEgressWithOwner_CombinesBasicAndCrossTenant(t *testing.T) {
	text := `owner: tenant-b key=sk-abcdefghijklmnopqrstuvwxyz123456`
	got := RedactEgressWithOwner(text, "tenant-a")
	if strings.Contains(got, "tenant-b") {
		t.Error("cross-tenant owner not redacted")
	}
	if strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Error("secret not redacted")
	}
}

// ─── SetEgressRedactionDisabled Tests ─────────────────────────────────────

// TestSetEgressRedactionDisabled_DisablesRedaction verifies that calling
// SetEgressRedactionDisabled(true) makes RedactEgress a no-op.
func TestSetEgressRedactionDisabled_DisablesRedaction(t *testing.T) {
	// Save and restore state
	defer SetEgressRedactionDisabled(false)

	SetEgressRedactionDisabled(true)
	input := `key=sk-abcdefghijklmnopqrstuvwxyz123456 email=admin@test.com`
	got := RedactEgress(input)
	if got != input {
		t.Errorf("RedactEgress should be a no-op when disabled, got %q", got)
	}
}

// TestSetEgressRedactionDisabled_ReEnablesRedaction verifies that calling
// SetEgressRedactionDisabled(false) re-enables redaction.
func TestSetEgressRedactionDisabled_ReEnablesRedaction(t *testing.T) {
	defer SetEgressRedactionDisabled(false)

	SetEgressRedactionDisabled(true)
	SetEgressRedactionDisabled(false)

	input := `key=sk-abcdefghijklmnopqrstuvwxyz123456`
	got := RedactEgress(input)
	if strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Error("RedactEgress should redact when re-enabled")
	}
}

// TestSetEgressRedactionDisabled_HotReload verifies the hot-reload pattern:
// disable → redact is no-op → re-enable → redact works again.
func TestSetEgressRedactionDisabled_HotReload(t *testing.T) {
	defer SetEgressRedactionDisabled(false)

	input := `key=sk-abcdefghijklmnopqrstuvwxyz123456`

	// Initially enabled (default)
	got := RedactEgress(input)
	if strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Error("RedactEgress should redact by default")
	}

	// Disable (simulating config reload with disable_egress_redaction=true)
	SetEgressRedactionDisabled(true)
	got = RedactEgress(input)
	if got != input {
		t.Errorf("RedactEgress should be no-op after disable, got %q", got)
	}

	// Re-enable (simulating config reload with disable_egress_redaction=false)
	SetEgressRedactionDisabled(false)
	got = RedactEgress(input)
	if strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Error("RedactEgress should redact after re-enable")
	}
}
