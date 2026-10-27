package core

import (
	"strings"
	"testing"
)

// TestRedactAndMarkUntrusted_CrossTenantRedaction_Wired verifies that
// redactAndMarkUntrusted threads the ownerID parameter to
// RedactEgressWithOwner, so cross-tenant OwnerID references in tool
// results are redacted. Regression test for the wiring (2026-10-25):
// RedactEgressWithOwner had zero callers; it is now wired via
// redactAndMarkUntrusted → RedactEgressWithOwner.
func TestRedactAndMarkUntrusted_CrossTenantRedaction_Wired(t *testing.T) {
	// Tool result containing another tenant's owner reference.
	toolResult := `owner:tenant_alice did something. owner:tenant_bob also referenced.`

	// With ownerID="tenant_alice", tenant_bob's reference should be redacted.
	redacted := redactAndMarkUntrusted(toolResult, "tenant_alice")
	if strings.Contains(redacted, "tenant_bob") {
		t.Errorf("cross-tenant reference 'tenant_bob' should be redacted, got: %s", redacted)
	}
	if !strings.Contains(redacted, "tenant_alice") {
		t.Errorf("own tenant reference 'tenant_alice' should be preserved, got: %s", redacted)
	}
	if !strings.Contains(redacted, "[OTHER_TENANT:REDACTED]") {
		t.Errorf("expected [OTHER_TENANT:REDACTED] marker, got: %s", redacted)
	}
}

// TestRedactAndMarkUntrusted_EmptyOwner_NoCrossTenantCheck verifies
// that when ownerID is empty (single-user mode), no cross-tenant
// redaction occurs — backward compatible with the old RedactEgress behavior.
func TestRedactAndMarkUntrusted_EmptyOwner_NoCrossTenantCheck(t *testing.T) {
	toolResult := `owner:tenant_alice did something. owner:tenant_bob also referenced.`

	redacted := redactAndMarkUntrusted(toolResult, "")
	if !strings.Contains(redacted, "tenant_alice") {
		t.Errorf("with empty owner, 'tenant_alice' should be preserved, got: %s", redacted)
	}
	if !strings.Contains(redacted, "tenant_bob") {
		t.Errorf("with empty owner, 'tenant_bob' should be preserved, got: %s", redacted)
	}
	if strings.Contains(redacted, "[OTHER_TENANT:REDACTED]") {
		t.Errorf("with empty owner, no cross-tenant redaction expected, got: %s", redacted)
	}
}

// TestSanitizePartialOutput_CrossTenantRedaction_Wired verifies that
// sanitizePartialOutputForPassthrough threads the ownerID parameter to
// RedactEgressWithOwner. Regression test for the wiring (2026-10-25).
func TestSanitizePartialOutput_CrossTenantRedaction_Wired(t *testing.T) {
	partialOutput := `owner:tenant_alice data. owner:tenant_bob leak.`

	sanitized := sanitizePartialOutputForPassthrough(partialOutput, "sieve_interception", "tenant_alice")
	if strings.Contains(sanitized, "tenant_bob") {
		t.Errorf("cross-tenant 'tenant_bob' should be redacted in sanitized output, got: %s", sanitized)
	}
	if !strings.Contains(sanitized, "[OTHER_TENANT:REDACTED]") {
		t.Errorf("expected [OTHER_TENANT:REDACTED] marker, got: %s", sanitized)
	}
}
