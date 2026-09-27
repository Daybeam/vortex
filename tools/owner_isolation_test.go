package tools

import (
	"context"
	"testing"
)

// helper: create a context with tier + owner set
func ctxWithAuth(tier, owner string) context.Context {
	ctx := context.Background()
	ctx = context.WithValue(ctx, AuthTierKey, tier)
	ctx = context.WithValue(ctx, AuthOwnerKey, owner)
	return ctx
}

func TestOwnerIsolation_CrossOwnerGetStatusReturnsNotFound(t *testing.T) {
	// Owner A's task; owner B tries to access — must be denied
	taskOwnerA := "alice"
	callerCtxB := ctxWithAuth(TierPublic, "bob")

	if CheckTaskOwnership(callerCtxB, taskOwnerA) {
		t.Error("owner B should NOT be able to access owner A's task")
	}
}

func TestOwnerIsolation_AdminTierBypassesCheck(t *testing.T) {
	// Admin can access any owner's task regardless of caller's OwnerID
	taskOwnerA := "alice"
	adminCtx := ctxWithAuth(TierAdmin, "bob") // admin is bob, task is alice's

	if !CheckTaskOwnership(adminCtx, taskOwnerA) {
		t.Error("admin tier should bypass ownership check")
	}
}

func TestOwnerIsolation_EmptyAPIKeysConfig_NoBehaviorChange(t *testing.T) {
	// When taskOwner is "" (no APIKeys configured), any caller passes
	callerCtx := ctxWithAuth(TierPublic, "bob")

	if !CheckTaskOwnership(callerCtx, "") {
		t.Error("empty task owner should always pass (backward compat)")
	}
}

func TestOwnerIsolation_SameOwnerDifferentKeysStillMatch(t *testing.T) {
	// Two keys sharing one OwnerID can both see the same tasks
	taskOwner := "alice"
	callerCtx1 := ctxWithAuth(TierPublic, "alice") // same owner, different key
	callerCtx2 := ctxWithAuth(TierPublic, "alice") // same owner, different key

	if !CheckTaskOwnership(callerCtx1, taskOwner) {
		t.Error("same owner (key 1) should pass")
	}
	if !CheckTaskOwnership(callerCtx2, taskOwner) {
		t.Error("same owner (key 2) should pass")
	}
}

func TestOwnerIsolation_CallerOwnerIDEmpty(t *testing.T) {
	// When caller has no OwnerID in context (e.g. legacy env-var key),
	// and task has an owner, access is denied (prevents unknown caller
	// from accessing owned tasks)
	taskOwner := "alice"
	legacyCtx := context.Background() // no AuthOwnerKey set
	legacyCtx = context.WithValue(legacyCtx, AuthTierKey, TierPublic)

	if CheckTaskOwnership(legacyCtx, taskOwner) {
		t.Error("legacy caller (empty OwnerID) should NOT access owned task")
	}
}

// ─── Per-connection identity (Phase 2) ──────────────────────────────────────

func TestComposeIdentity_BothPresent(t *testing.T) {
	got := composeIdentity("alice", "sess-xyz")
	if got != "alice:sess-xyz" {
		t.Errorf("expected 'alice:sess-xyz', got %q", got)
	}
}

func TestComposeIdentity_OnlyOwner(t *testing.T) {
	got := composeIdentity("alice", "")
	if got != "alice" {
		t.Errorf("expected 'alice', got %q", got)
	}
}

func TestComposeIdentity_OnlySession(t *testing.T) {
	got := composeIdentity("", "sess-xyz")
	if got != "sess-xyz" {
		t.Errorf("expected 'sess-xyz', got %q", got)
	}
}

func TestComposeIdentity_Neither(t *testing.T) {
	got := composeIdentity("", "")
	if got != "" {
		t.Errorf("expected '', got %q", got)
	}
}

func TestPerConnectionIsolation_SameKeyDifferentSessions(t *testing.T) {
	// Two callers share the same API key (OwnerID="team-key") but have
	// different MCP session IDs. Their composite identities must differ,
	// so they can't access each other's tasks.
	identity1 := composeIdentity("team-key", "sess-alice")
	identity2 := composeIdentity("team-key", "sess-bob")

	if identity1 == identity2 {
		t.Error("different sessions with same key should produce different identities")
	}
	if identity1 != "team-key:sess-alice" {
		t.Errorf("expected 'team-key:sess-alice', got %q", identity1)
	}
	if identity2 != "team-key:sess-bob" {
		t.Errorf("expected 'team-key:sess-bob', got %q", identity2)
	}
}

func TestPerConnectionIsolation_SameSessionSameKey(t *testing.T) {
	// Same key + same session → same identity → can access each other's tasks
	identity1 := composeIdentity("team-key", "sess-alice")
	identity2 := composeIdentity("team-key", "sess-alice")

	if identity1 != identity2 {
		t.Error("same session + same key should produce same identity")
	}
}

func TestPerConnectionIsolation_NoSessionFallsBackToOwner(t *testing.T) {
	// Without an MCP session, identity falls back to just the OwnerID (Phase 1)
	identity := composeIdentity("alice", "")
	if identity != "alice" {
		t.Errorf("expected 'alice' (Phase 1 fallback), got %q", identity)
	}
}
