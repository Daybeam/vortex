package core

import (
	"testing"
)

func TestDomainToolReadCache_PutGet(t *testing.T) {
	c := NewDomainToolReadCache()
	args := map[string]any{"reservation_id": "MFRB94"}
	if _, ok := c.Get("airline", "get_reservation_details", args); ok {
		t.Fatal("expected miss before Put")
	}
	c.Put("airline", "get_reservation_details", args, "[cached markdown]")
	got, ok := c.Get("airline", "get_reservation_details", args)
	if !ok {
		t.Fatal("expected hit after Put")
	}
	if got != "[cached markdown]" {
		t.Fatalf("unexpected value: %q", got)
	}
}

func TestDomainToolReadCache_ArgsAndMCPScoping(t *testing.T) {
	c := NewDomainToolReadCache()
	base := map[string]any{"reservation_id": "MFRB94"}

	// different args -> miss
	c.Put("airline", "get_reservation_details", base, "v1")
	if _, ok := c.Get("airline", "get_reservation_details", map[string]any{"reservation_id": "OTHER"}); ok {
		t.Fatal("different args should miss")
	}
	// different mcpID -> miss
	if _, ok := c.Get("banking", "get_reservation_details", base); ok {
		t.Fatal("different mcpID should miss")
	}
	// different tool name -> miss
	if _, ok := c.Get("airline", "list_reservations", base); ok {
		t.Fatal("different tool should miss")
	}
	// same -> hit
	if _, ok := c.Get("airline", "get_reservation_details", base); !ok {
		t.Fatal("same key should hit")
	}
}

func TestDomainToolReadCache_Invalidate(t *testing.T) {
	c := NewDomainToolReadCache()
	args := map[string]any{"id": "X"}
	c.Put("s1", "get_a", args, "r1")
	c.Put("s2", "get_a", args, "r2")

	c.Invalidate("s1")
	if _, ok := c.Get("s1", "get_a", args); ok {
		t.Fatal("s1 should be invalidated")
	}
	// s2 untouched
	if _, ok := c.Get("s2", "get_a", args); !ok {
		t.Fatal("s2 should survive s1 invalidation")
	}
}

func TestDomainToolReadCache_Clear(t *testing.T) {
	c := NewDomainToolReadCache()
	c.Put("s1", "get_a", map[string]any{"x": 1}, "r")
	c.Put("s2", "get_b", map[string]any{"x": 2}, "r")
	c.Clear()
	if _, ok := c.Get("s1", "get_a", map[string]any{"x": 1}); ok {
		t.Fatal("Clear should remove all entries")
	}
	if _, ok := c.Get("s2", "get_b", map[string]any{"x": 2}); ok {
		t.Fatal("Clear should remove all entries")
	}
}

func TestDomainToolReadCache_KeyOrderIndependent(t *testing.T) {
	c := NewDomainToolReadCache()
	c.Put("s", "get_t", map[string]any{"a": 1, "b": 2}, "r")
	// JSON marshal sorts keys, so reversed insertion order must still hit
	if _, ok := c.Get("s", "get_t", map[string]any{"b": 2, "a": 1}); !ok {
		t.Fatal("key computation must be order-independent")
	}
}
