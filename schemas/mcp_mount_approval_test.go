package schemas

import (
	"encoding/json"
	"testing"
)

// TestApprovalRecord_BackwardCompatAndRoundTrip (§2, 2026-10-24): the
// structured ApprovalRecord must read legacy bare-bool data (`true`) AND the new
// object form, and survive a graph round-trip.
func TestApprovalRecord_BackwardCompatAndRoundTrip(t *testing.T) {
	// Legacy bare-bool form.
	var r ApprovalRecord
	if err := json.Unmarshal([]byte("true"), &r); err != nil {
		t.Fatalf("legacy bool unmarshal: %v", err)
	}
	if !r.Approved {
		t.Fatal("legacy true must yield Approved=true")
	}

	// New structured form.
	var r2 ApprovalRecord
	if err := json.Unmarshal([]byte(`{"approved":true,"scope":"mount"}`), &r2); err != nil {
		t.Fatalf("struct unmarshal: %v", err)
	}
	if !r2.Approved || r2.Scope != "mount" {
		t.Fatalf("unexpected: %+v", r2)
	}

	// Round-trip a graph with the new map.
	g := &TaskGraph{}
	g.ApproveMCPMounts([]string{"m1"}, "", "")
	data, _ := json.Marshal(g)
	var g2 TaskGraph
	if err := json.Unmarshal(data, &g2); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if !g2.IsMCPMountApproved("m1") {
		t.Fatal("round-trip must preserve the approval")
	}

	// Legacy graph with a bare-bool map (old persisted data).
	var g3 TaskGraph
	if err := json.Unmarshal([]byte(`{"approved_mcp_mounts":{"m2":true}}`), &g3); err != nil {
		t.Fatalf("legacy map unmarshal: %v", err)
	}
	if !g3.IsMCPMountApproved("m2") {
		t.Fatal("legacy bare-bool map must be read as approved")
	}
}
