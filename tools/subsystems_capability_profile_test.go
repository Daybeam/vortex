package tools

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/store"
	_ "modernc.org/sqlite"
)

// Regression tests for Q5-1 (register O7, answered 2026-11-05).
//
// Bug shape: the routing telemetry was one-way. Four construction sites build a
// CapabilityProfileStore, the spawner writes an outcome into it after every
// spawn, and NextPareto reads it to pick a provider -- but no surface could ask
// what it had learned. "Is the router actually learning anything?" was
// answerable only by opening the SQLite file by hand.
//
// The operator's ruling: 「加只读的 exp.get_capability_profile，成本是 1 个 action 加
// 1 个 handler」. Read-only, one action, one handler -- and the part these tests
// pin hardest: a process that never wired the routing must say "not wired", not
// return an empty list. An empty list reads as "no runs yet", which is a
// different fact and would have hidden the runHub gap this batch surfaced.

func newCapabilityProfileApp(t *testing.T, withDB bool) *App {
	t.Helper()
	app := &App{Tier: TierAdmin}
	if !withDB {
		registerExperienceSubsystem(app)
		return app
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "caps.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS model_capability_profiles (
		model_id       TEXT NOT NULL,
		capability     TEXT NOT NULL,
		total_runs     INTEGER DEFAULT 0,
		success_count  INTEGER DEFAULT 0,
		avg_turns_used REAL DEFAULT 0,
		avg_latency_ms REAL DEFAULT 0,
		avg_token_cost REAL DEFAULT 0,
		theta          REAL DEFAULT 0,
		last_updated   DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (model_id, capability)
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	app.CapProfile = store.NewCapabilityProfileStore(db)
	registerExperienceSubsystem(app)
	return app
}

func seedProfile(t *testing.T, app *App, modelID, capability string, runs, successes int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < runs; i++ {
		if err := app.CapProfile.RecordOutcome(ctx, modelID, capability, i < successes, 5, 1200, 900); err != nil {
			t.Fatalf("RecordOutcome(%s/%s): %v", modelID, capability, err)
		}
	}
}

func callProfileQuery(t *testing.T, app *App, args map[string]any) map[string]any {
	t.Helper()
	res, err := registry.Invoke(adminCtx(), app, "exp", "get_capability_profile", args)
	if err != nil {
		t.Fatalf("exp.get_capability_profile: %v", err)
	}
	out, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want map", res)
	}
	return out
}

func profilesOf(t *testing.T, out map[string]any) []store.ModelCapabilityProfile {
	t.Helper()
	p, ok := out["profiles"].([]store.ModelCapabilityProfile)
	if !ok {
		t.Fatalf("profiles is %T, want []store.ModelCapabilityProfile", out["profiles"])
	}
	return p
}

// TestCapabilityProfile_NotWiredSaysSo is the hub case (main.go runHub never
// calls WireCapabilityRouting): the answer must distinguish "this process
// records nothing" from "nothing has been recorded yet".
func TestCapabilityProfile_NotWiredSaysSo(t *testing.T) {
	app := newCapabilityProfileApp(t, false)
	out := callProfileQuery(t, app, nil)
	if out["wired"] != false {
		t.Fatalf("wired = %v, want false", out["wired"])
	}
	if _, hasList := out["profiles"]; hasList {
		t.Error("an unwired process must not answer with a profiles list")
	}
	if reason, _ := out["reason"].(string); reason == "" {
		t.Error("not-wired answer carries no reason, so a caller cannot act on it")
	}
}

func TestCapabilityProfile_QueryShapes(t *testing.T) {
	app := newCapabilityProfileApp(t, true)
	seedProfile(t, app, "model-a", "writes Go code", 5, 4)
	seedProfile(t, app, "model-a", "reads logs", 2, 2)
	seedProfile(t, app, "model-b", "writes Go code", 9, 3)

	t.Run("all", func(t *testing.T) {
		out := callProfileQuery(t, app, map[string]any{})
		if out["wired"] != true {
			t.Fatalf("wired = %v, want true", out["wired"])
		}
		got := profilesOf(t, out)
		if len(got) != 3 {
			t.Fatalf("profiles = %d, want 3", len(got))
		}
		// Most-evidenced first, so a truncated page keeps what has samples.
		if got[0].TotalRuns != 9 || got[1].TotalRuns != 5 || got[2].TotalRuns != 2 {
			t.Errorf("not ordered by runs desc: %+v", got)
		}
	})

	t.Run("by_model", func(t *testing.T) {
		out := callProfileQuery(t, app, map[string]any{"model_id": "model-a"})
		got := profilesOf(t, out)
		if len(got) != 2 {
			t.Fatalf("profiles = %d, want 2 (model-a's two capabilities)", len(got))
		}
		for _, p := range got {
			if p.ModelID != "model-a" {
				t.Errorf("leaked %s into a model-a query", p.ModelID)
			}
		}
	})

	t.Run("by_model_and_capability", func(t *testing.T) {
		out := callProfileQuery(t, app, map[string]any{"model_id": "model-b", "capability": "writes Go code"})
		got := profilesOf(t, out)
		if len(got) != 1 {
			t.Fatalf("profiles = %d, want exactly 1", len(got))
		}
		if got[0].SuccessRate != 3.0/9.0 {
			t.Errorf("success_rate = %v, want the recorded 3/9", got[0].SuccessRate)
		}
	})

	// The other side of the NotWired test: an empty list is reserved for "this
	// key has no runs", so it must stay reachable and stay distinct.
	t.Run("unknown_key_is_empty_not_unwired", func(t *testing.T) {
		out := callProfileQuery(t, app, map[string]any{"model_id": "model-zzz"})
		if out["wired"] != true {
			t.Fatalf("wired = %v, want true (the store is wired, the key is just absent)", out["wired"])
		}
		if n, _ := out["profile_count"].(int); n != 0 {
			t.Errorf("profile_count = %v, want 0", out["profile_count"])
		}
		if got := profilesOf(t, out); len(got) != 0 {
			t.Errorf("profiles = %d, want 0", len(got))
		}
	})
}

// TestCapabilityProfile_ReadsLikeTheRouter: the query face is only worth having
// if it reports the same numbers NextPareto and the IRT signals consume. theta is
// reported as stored and must not be dressed up -- RecalculateThetas has no
// production caller, so 0 is what the router sees.
func TestCapabilityProfile_ReportsThetaAsStored(t *testing.T) {
	app := newCapabilityProfileApp(t, true)
	seedProfile(t, app, "model-a", "writes Go code", 3, 3)
	got := profilesOf(t, callProfileQuery(t, app, map[string]any{"model_id": "model-a"}))
	if len(got) != 1 {
		t.Fatalf("profiles = %d, want 1", len(got))
	}
	if got[0].Theta != 0 {
		t.Errorf("theta = %v, want the stored 0 -- the panel must not invent an estimate", got[0].Theta)
	}
}

func TestCapabilityProfilePage_DropsTheLeastEvidenced(t *testing.T) {
	profiles := make([]store.ModelCapabilityProfile, 60)
	for i := range profiles {
		profiles[i] = store.ModelCapabilityProfile{
			ModelID: "m", Capability: string(rune('a' + i)), TotalRuns: i + 1,
		}
	}
	page, dropped := pageCapabilityProfiles(profiles, capabilityProfilePageLimit)
	if len(page) != capabilityProfilePageLimit {
		t.Fatalf("page = %d, want %d", len(page), capabilityProfilePageLimit)
	}
	if dropped != 10 {
		t.Errorf("dropped = %d, want 10 (the caller must be told)", dropped)
	}
	if page[0].TotalRuns != 60 || page[capabilityProfilePageLimit-1].TotalRuns != 11 {
		t.Errorf("page should keep runs 60..11, got %d..%d", page[0].TotalRuns, page[capabilityProfilePageLimit-1].TotalRuns)
	}
}
