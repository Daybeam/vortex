package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
	"github.com/daybeam/vortex/tools"
)

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — REST schedule
// mutations fetched by ID alone, and run-now discarded the caller identity.
func TestScheduleREST_OwnerAuthorization(t *testing.T) {
	app := &tools.App{OwnerIsolation: true, Tier: tools.TierPublic,
		SStore: store.NewScheduleStore(filepath.Join(t.TempDir(), "schedules.json"), nil)}
	app.Schedule = core.NewScheduleManager(nil, nil, app.SStore, nil)
	h := &APIHandler{App: app}
	for _, sch := range []*schemas.Schedule{
		{ID: "alice", OwnerID: "alice", Name: "alice", Type: schemas.ScheduleInterval, Interval: "1h", Status: schemas.ScheduleActive, NextRun: time.Now().Add(time.Hour), TaskInputs: []schemas.StepInput{{Task: "work"}}},
		{ID: "bob", OwnerID: "bob", Name: "bob", Type: schemas.ScheduleInterval, Interval: "1h", Status: schemas.ScheduleActive, NextRun: time.Now().Add(time.Hour), TaskInputs: []schemas.StepInput{{Task: "work"}}},
		{ID: "legacy", Name: "legacy", Type: schemas.ScheduleInterval, Interval: "1h", Status: schemas.ScheduleActive, NextRun: time.Now().Add(time.Hour), TaskInputs: []schemas.StepInput{{Task: "work"}}},
	} {
		if err := app.SStore.Set(sch); err != nil {
			t.Fatal(err)
		}
	}
	alice := context.WithValue(context.Background(), tools.AuthOwnerKey, "alice")
	alice = context.WithValue(alice, tools.AuthTierKey, tools.TierPublic)

	rec := httptest.NewRecorder()
	h.SchedulesHandler(rec, httptest.NewRequest(http.MethodGet, "/api/schedules", nil).WithContext(alice))
	var listed []*schemas.Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "alice" {
		t.Fatalf("alice REST list = %+v", listed)
	}

	for _, tc := range []struct {
		name, body string
		handler    http.HandlerFunc
	}{
		{"pause", `{"id":"bob"}`, h.SchedulePauseHandler},
		{"delete", `{"id":"bob"}`, h.ScheduleDeleteHandler},
		{"update", `{"id":"bob","name":"stolen","type":"interval","interval":"1h","task_inputs":[{"task":"work"}]}`, h.ScheduleUpdateHandler},
		{"run", `{"id":"bob"}`, h.ScheduleRunNowHandler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler(rec, httptest.NewRequest(http.MethodPost, "/api/schedules/"+tc.name, strings.NewReader(tc.body)).WithContext(alice))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("cross-owner status=%d body=%s, want 404", rec.Code, rec.Body.String())
			}
		})
	}
	if got := app.SStore.Get("bob"); got == nil || got.Name != "bob" || got.Status != schemas.ScheduleActive {
		t.Fatalf("cross-owner mutation changed bob: %+v", got)
	}

	rec = httptest.NewRecorder()
	h.ScheduleUpdateHandler(rec, httptest.NewRequest(http.MethodPost, "/api/schedules/update", strings.NewReader(
		`{"id":"alice","owner_id":"bob","submitter_is_admin":true,"name":"alice-edited","type":"interval","interval":"1h","task_inputs":[{"task":"work","submitter_is_admin":true}]}`)).WithContext(alice))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner update status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := app.SStore.Get("alice")
	if got.OwnerID != "alice" || got.SubmitterIsAdmin {
		t.Fatalf("update forged immutable owner/admin: %+v", got)
	}

	rec = httptest.NewRecorder()
	h.ScheduleCreateHandler(rec, httptest.NewRequest(http.MethodPost, "/api/schedules/create", strings.NewReader(
		`{"name":"new","type":"interval","interval":"1h","owner_id":"bob","task_inputs":[{"task":"work","submitter_is_admin":true}]}`)).WithContext(alice))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if got := app.SStore.Get(created.ID); got.OwnerID != "alice" || got.SubmitterIsAdmin {
		t.Fatalf("create forged owner/admin: %+v", got)
	}

	anonymous := context.WithValue(context.Background(), tools.AuthTierKey, tools.TierPublic)
	rec = httptest.NewRecorder()
	h.ScheduleCreateHandler(rec, httptest.NewRequest(http.MethodPost, "/api/schedules/create", strings.NewReader(
		`{"name":"anonymous","type":"interval","interval":"1h","task_inputs":[{"task":"work"}]}`)).WithContext(anonymous))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous create status=%d, want 403", rec.Code)
	}

	app.OwnerIsolation = false
	rec = httptest.NewRecorder()
	h.SchedulesHandler(rec, httptest.NewRequest(http.MethodGet, "/api/schedules", nil).WithContext(anonymous))
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 4 {
		t.Fatalf("local positive control returned %d schedules, want 4", len(listed))
	}
}

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — a caller-supplied
// create ID must never overwrite another owner's schedule.
func TestScheduleCreateHandler_DoesNotOverwriteExistingID(t *testing.T) {
	app := &tools.App{OwnerIsolation: true, Tier: tools.TierPublic,
		SStore: store.NewScheduleStore(filepath.Join(t.TempDir(), "schedules.json"), nil)}
	bob := &schemas.Schedule{ID: "bob-schedule", Name: "bob", OwnerID: "bob", TaskInputs: []schemas.StepInput{{Task: "work"}}}
	if err := app.SStore.Set(bob); err != nil {
		t.Fatal(err)
	}
	h := &APIHandler{App: app}
	alice := context.WithValue(context.Background(), tools.AuthOwnerKey, "alice")
	alice = context.WithValue(alice, tools.AuthTierKey, tools.TierPublic)
	rec := httptest.NewRecorder()
	h.ScheduleCreateHandler(rec, httptest.NewRequest(http.MethodPost, "/api/schedules/create", strings.NewReader(
		`{"id":"bob-schedule","name":"stolen","type":"interval","interval":"1h","task_inputs":[{"task":"work"}]}`)).WithContext(alice))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.ID == bob.ID {
		t.Fatalf("create reused caller ID %q", created.ID)
	}
	got := app.SStore.Get(bob.ID)
	if got == nil || got.Name != "bob" || got.OwnerID != "bob" {
		t.Fatalf("create overwrote another owner's schedule: %+v", got)
	}
}
