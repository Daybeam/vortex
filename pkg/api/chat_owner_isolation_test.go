package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
	"github.com/daybeam/vortex/tools"
)

// Regression coverage for docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md.
func ownerIsolationHandler(t *testing.T) *ChatHandler {
	t.Helper()
	return &ChatHandler{
		App:     &tools.App{OwnerIsolation: true, Tier: tools.TierPublic},
		Harness: &chat.ChatHarness{},
		Store:   chat.NewChatSessionStore(t.TempDir(), nil),
	}
}

func ownerAuthContext(owner, tier string) context.Context {
	ctx := context.WithValue(context.Background(), tools.AuthOwnerKey, owner)
	return context.WithValue(ctx, tools.AuthTierKey, tier)
}

func createOwnedSession(t *testing.T, h *ChatHandler, id, owner string, shared bool) *chat.ChatSession {
	t.Helper()
	s, err := h.Store.GetOrCreateOwned(id, owner, true, func(*chat.ChatSession) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	s.SetShared(shared)
	s.AppendUserMessage("user-"+id, "private message for "+owner, "", "")
	if err := h.Store.Persist(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestChatOwnerIsolation_ProcessMessageAssignmentAndDenials(t *testing.T) {
	h := ownerIsolationHandler(t)
	resp := h.ProcessMessage(ownerAuthContext("alice", tools.TierPublic), ChatRequest{
		Content: "/live status", OwnerID: "alice", UserTier: TierAuthorized,
	})
	if resp.StatusCode != 0 || !resp.SlashCommand {
		t.Fatalf("owner create failed: %+v", resp)
	}
	created := h.Store.Get(resp.SessionID)
	if created == nil || created.GetOwnerID() != "alice" {
		t.Fatalf("new session owner = %+v, want alice", created)
	}

	createOwnedSession(t, h, "alice-session", "alice", false)
	resp = h.ProcessMessage(ownerAuthContext("bob", tools.TierPublic), ChatRequest{
		SessionID: "alice-session", Content: "/live status", OwnerID: "bob", UserTier: TierAuthorized,
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner send = %+v, want 404", resp)
	}

	legacy := h.Store.GetOrCreate("legacy-ownerless")
	if err := h.Store.Persist(legacy); err != nil {
		t.Fatal(err)
	}
	resp = h.ProcessMessage(ownerAuthContext("alice", tools.TierPublic), ChatRequest{
		SessionID: legacy.ID, Content: "/live status", OwnerID: "alice", UserTier: TierAuthorized,
	})
	if resp.StatusCode != http.StatusNotFound || legacy.GetOwnerID() != "" {
		t.Fatalf("ownerless session was exposed or claimed: resp=%+v owner=%q", resp, legacy.GetOwnerID())
	}
	legacy.SetShared(true)
	resp = h.ProcessMessage(ownerAuthContext("guest", tools.TierPublic), ChatRequest{
		SessionID: legacy.ID, Content: "/live status", OwnerID: "guest", UserTier: TierGuest,
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("ownerless shared session exposed in multi-user mode: %+v", resp)
	}
	legacy.SetShared(false)

	resp = h.ProcessMessage(ownerAuthContext("admin", tools.TierAdmin), ChatRequest{
		SessionID: legacy.ID, Content: "/live status", OwnerID: "admin", UserTier: TierAdmin,
	})
	if resp.StatusCode != 0 || !resp.SlashCommand {
		t.Fatalf("admin positive control failed: %+v", resp)
	}
}

func TestChatOwnerIsolation_SharedGuestAndStableLiveOwner(t *testing.T) {
	h := ownerIsolationHandler(t)
	createOwnedSession(t, h, "shared", "alice", true)
	resp := h.ProcessMessage(ownerAuthContext("guest", tools.TierPublic), ChatRequest{
		SessionID: "shared", Content: "/live status", OwnerID: "guest", UserTier: TierGuest,
	})
	if resp.StatusCode != 0 || !resp.SlashCommand {
		t.Fatalf("shared guest positive control failed: %+v", resp)
	}
	if owner := h.Store.Get("shared").GetOwnerID(); owner != "alice" {
		t.Fatalf("guest transferred shared owner to %q", owner)
	}

	resp = h.ProcessMessage(ownerAuthContext("admin", tools.TierAdmin), ChatRequest{
		SessionID: "shared", Content: "/live start", OwnerID: "admin", CallerID: "admin", UserTier: TierAdmin,
	})
	if resp.StatusCode != 0 || !resp.SlashCommand {
		t.Fatalf("admin live start failed: %+v", resp)
	}
	if owner := h.Store.Get("shared").GetOwnerID(); owner != "alice" {
		t.Fatalf("live start overwrote stable owner with %q", owner)
	}
}

func TestChatOwnerIsolation_HTTPReadEndpointsAndOwnerPayload(t *testing.T) {
	sessionDir := t.TempDir()
	h := &ChatHandler{
		App:     &tools.App{OwnerIsolation: true, Tier: tools.TierPublic},
		Harness: &chat.ChatHarness{},
		Store:   chat.NewChatSessionStore(sessionDir, nil),
	}
	owned := createOwnedSession(t, h, "http-session", "alice", false)
	messageID := owned.SnapshotMessages()[0].ID
	paths := []string{
		"/api/chat/history?session_id=http-session",
		fmt.Sprintf("/api/chat/messages/meta?session_id=http-session&ids=%s", messageID),
		fmt.Sprintf("/api/chat/history/anchor?session_id=http-session&target_id=%s", messageID),
		"/api/chat/stream?session_id=http-session",
		"/api/chat/artifacts?session_id=http-session",
		"/api/chat/artifact?session_id=http-session&name=secret.txt",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			// Dispatch through the real endpoint for this path.
			w := httptest.NewRecorder()
			ctx, cancel := context.WithTimeout(ownerAuthContext("bob", tools.TierPublic), time.Second)
			defer cancel()
			r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
			switch {
			case strings.Contains(path, "/messages/meta"):
				h.MetaHandler(w, r)
			case strings.Contains(path, "/history/anchor"):
				h.AnchorHandler(w, r)
			case strings.Contains(path, "/history"):
				h.HistoryHandler(w, r)
			case strings.Contains(path, "/artifacts"):
				h.ArtifactsHandler(w, r)
			case strings.Contains(path, "/artifact?"):
				h.ArtifactHandler(w, r)
			default:
				h.StreamHandler(w, r)
			}
			if w.Code != http.StatusNotFound {
				t.Fatalf("cross-owner read returned %d, want 404", w.Code)
			}
		})
	}

	// Force a disk reload so the positive control also exercises persisted owner metadata.
	h.Store = chat.NewChatSessionStore(sessionDir, nil)
	w := httptest.NewRecorder()
	h.HistoryHandler(w, httptest.NewRequest(http.MethodGet, paths[0], nil).WithContext(ownerAuthContext("alice", tools.TierPublic)))
	if w.Code != http.StatusOK {
		t.Fatalf("owner history positive control returned %d: %s", w.Code, w.Body.String())
	}

	body := `{"content":"/live status","owner_id":"bob"}`
	w = httptest.NewRecorder()
	h.SendHandler(w, httptest.NewRequest(http.MethodPost, "/api/chat/send", strings.NewReader(body)).WithContext(ownerAuthContext("alice", tools.TierPublic)))
	if w.Code != http.StatusOK {
		t.Fatalf("send positive control returned %d: %s", w.Code, w.Body.String())
	}
	var sent ChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	if owner := h.Store.Get(sent.SessionID).GetOwnerID(); owner != "alice" {
		t.Fatalf("HTTP payload forged owner %q; want alice", owner)
	}
}

func TestChatOwnerIsolation_SessionsListFiltersBeforePagination(t *testing.T) {
	h := ownerIsolationHandler(t)
	for i := 0; i < 4; i++ {
		createOwnedSession(t, h, fmt.Sprintf("bob-%d", i), "bob", false)
	}
	for i := 0; i < 2; i++ {
		createOwnedSession(t, h, fmt.Sprintf("alice-%d", i), "alice", false)
	}
	h.Store.GetOrCreate("ownerless")
	if err := h.Store.Persist(h.Store.Get("ownerless")); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.SessionsHandler(w, httptest.NewRequest(http.MethodGet, "/api/chat/sessions?limit=1&offset=1", nil).WithContext(ownerAuthContext("alice", tools.TierPublic)))
	if w.Code != http.StatusOK {
		t.Fatalf("sessions list returned %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Sessions) != 1 || !strings.HasPrefix(response.Sessions[0].ID, "alice-") {
		t.Fatalf("foreign rows crowded out or leaked into owned page: %+v", response.Sessions)
	}
	w = httptest.NewRecorder()
	h.SessionsHandler(w, httptest.NewRequest(http.MethodGet, "/api/chat/sessions", nil).WithContext(ownerAuthContext("", tools.TierPublic)))
	var anonymous struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &anonymous); err != nil {
		t.Fatal(err)
	}
	if len(anonymous.Sessions) != 0 {
		t.Fatalf("anonymous multi-user caller listed ownerless/foreign sessions: %+v", anonymous.Sessions)
	}
}

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — sharing a
// session must not lend the guest the owner's schedule visibility.
func TestChatOwnerIsolation_SharedScheduleUsesCallerOwner(t *testing.T) {
	h := ownerIsolationHandler(t)
	h.App.SStore = store.NewScheduleStore(filepath.Join(t.TempDir(), "schedules.json"), nil)
	createOwnedSession(t, h, "shared-schedules", "alice", true)
	for _, sch := range []*schemas.Schedule{
		{ID: "alice-private", Name: "alice-private", OwnerID: "alice"},
		{ID: "guest-visible", Name: "guest-visible", OwnerID: "guest"},
	} {
		if err := h.App.SStore.Set(sch); err != nil {
			t.Fatal(err)
		}
	}
	resp := h.ProcessMessage(ownerAuthContext("guest", tools.TierPublic), ChatRequest{
		SessionID: "shared-schedules", Content: "/schedule", OwnerID: "guest", UserTier: TierGuest,
	})
	if resp.StatusCode != 0 || !resp.SlashCommand {
		t.Fatalf("shared schedule command failed: %+v", resp)
	}
	messages := h.Store.Get("shared-schedules").SnapshotMessages()
	got := messages[len(messages)-1].Content
	if strings.Contains(got, "alice-private") || !strings.Contains(got, "guest-visible") {
		t.Fatalf("shared guest schedule scope = %q", got)
	}
}

type scheduleToolProvider struct {
	inputs chan string
	calls  int
}

func (p *scheduleToolProvider) Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error) {
	return p.StreamComplete(ctx, req, func(string) error { return nil })
}

func (p *scheduleToolProvider) StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(string) error) (*schemas.ProviderResponse, error) {
	p.inputs <- req.User
	p.calls++
	if p.calls == 1 {
		return &schemas.ProviderResponse{ToolCalls: []schemas.ToolCall{{Name: "schedule_list", Arguments: map[string]any{}}}}, nil
	}
	onChunk("done")
	return &schemas.ProviderResponse{Text: "done"}, nil
}

func (p *scheduleToolProvider) Embed(context.Context, string) ([]float32, error) { return nil, nil }
func (p *scheduleToolProvider) CountTokens(context.Context, string) (int, error) { return 0, nil }
func (p *scheduleToolProvider) Name() string                                   { return "schedule-tool-test" }

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — the chat
// schedule tool executor must capture the stable caller before replacing the
// request context. A shared session's approval-context override must not lend
// the guest the owner's schedule visibility.
func TestChatOwnerIsolation_ScheduleToolExecutorUsesCaller(t *testing.T) {
	h := ownerIsolationHandler(t)
	h.App.SStore = store.NewScheduleStore(filepath.Join(t.TempDir(), "schedules.json"), nil)
	createOwnedSession(t, h, "shared-tool-schedules", "alice", true)
	for _, sch := range []*schemas.Schedule{
		{ID: "alice-private", Name: "alice-private", OwnerID: "alice"},
		{ID: "guest-visible", Name: "guest-visible", OwnerID: "guest"},
	} {
		if err := h.App.SStore.Set(sch); err != nil {
			t.Fatal(err)
		}
	}
	h.App.Registry = &config.Registry{}
	tools.RegisterSubsystems(h.App)
	provider := &scheduleToolProvider{inputs: make(chan string, 2)}
	h.Harness = &chat.ChatHarness{Provider: provider, Model: "schedule-tool-test", MaxTurns: 3}
	defer h.Shutdown()
	resp := h.ProcessMessage(ownerAuthContext("guest", tools.TierPublic), ChatRequest{
		SessionID: "shared-tool-schedules", MessageID: "schedule-tool-1", Content: "list schedules", OwnerID: "guest", UserTier: TierGuest,
	})
	if resp.StatusCode != 0 || resp.Error != "" {
		t.Fatalf("schedule tool turn failed: %+v", resp)
	}
	select {
	case <-provider.inputs:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not make the first schedule-tool call")
	}
	var toolResult string
	select {
	case toolResult = <-provider.inputs:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not receive the schedule tool result")
	}
	if strings.Contains(toolResult, "alice-private") || !strings.Contains(toolResult, "guest-visible") {
		t.Fatalf("chat schedule tool used the wrong owner: %s", toolResult)
	}
}

func TestChatOwnerIsolation_UploadServeAccess(t *testing.T) {
	db, err := store.InitDB(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := store.NewSQLiteChatBackend(db)
	if err := backend.SaveSession(context.Background(), "owned", "", "", "alice", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := backend.SaveSession(context.Background(), "shared", "", "", "alice", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := backend.SaveSession(context.Background(), "legacy", "", "", "", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	h := &APIHandler{App: &tools.App{DB: db, OwnerIsolation: true, Tier: tools.TierPublic}}
	for _, tt := range []struct {
		id, owner, tier string
		want            bool
	}{
		{"owned", "alice", tools.TierPublic, true},
		{"owned", "bob", tools.TierPublic, false},
		{"shared", "guest", tools.TierPublic, true},
		{"legacy", "guest", tools.TierPublic, false},
		{"owned", "admin", tools.TierAdmin, true},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/uploads/"+tt.id+"/image.png", nil).WithContext(ownerAuthContext(tt.owner, tt.tier))
		if got := h.chatUploadSessionAllowed(r, tt.id); got != tt.want {
			t.Errorf("upload %s as %s/%s = %v, want %v", tt.id, tt.owner, tt.tier, got, tt.want)
		}
	}
}

func TestChatOwnerIsolation_LegacyLocalCompatibility(t *testing.T) {
	h := ownerIsolationHandler(t)
	h.App.OwnerIsolation = false
	resp := h.ProcessMessage(context.Background(), ChatRequest{Content: "/live status"})
	if resp.StatusCode != 0 || !resp.SlashCommand {
		t.Fatalf("local ownerless compatibility failed: %+v", resp)
	}
	if owner := h.Store.Get(resp.SessionID).GetOwnerID(); owner != "" {
		t.Fatalf("local ownerless session acquired owner %q", owner)
	}
}

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — multi-user
// uploads must not create ownerless sessions, and a new upload owns its session.
func TestChatOwnerIsolation_MultiUserUploadCreatesOwnedSession(t *testing.T) {
	h := ownerIsolationHandler(t)
	t.Chdir(t.TempDir())
	upload := func(ctx context.Context, sessionID string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("files", "owned.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("png-data")); err != nil {
			t.Fatal(err)
		}
		if sessionID != "" {
			if err := form.WriteField("session_id", sessionID); err != nil {
				t.Fatal(err)
			}
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/chat/upload", &body).WithContext(ctx)
		req.Header.Set("Content-Type", form.FormDataContentType())
		rec := httptest.NewRecorder()
		h.UploadHandler(rec, req)
		return rec
	}

	rec := upload(ownerAuthContext("", tools.TierPublic), "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous upload status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	rec = upload(ownerAuthContext("alice", tools.TierPublic), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owned upload status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var response struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	session := h.Store.Load(response.SessionID)
	if session == nil || session.GetOwnerID() != "alice" {
		t.Fatalf("upload session = %+v, want durable alice owner", session)
	}
	rec = upload(ownerAuthContext("bob", tools.TierPublic), response.SessionID)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner upload status=%d, want 404", rec.Code)
	}
}

func TestChatOwnerIsolation_LegacyLocalArtifactAndUploadPaths(t *testing.T) {
	h := ownerIsolationHandler(t)
	h.App.OwnerIsolation = false
	h.Harness.OutputBase = "outputs"
	t.Chdir(t.TempDir())

	// Legacy clients may upload before the first message creates a durable row.
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "local.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("png-data")); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/chat/upload?session_id=not-created", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.UploadHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy local upload = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	baseFile := filepath.Join("outputs", "legacy.txt")
	if err := os.WriteFile(baseFile, []byte("legacy"), 0644); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/chat/artifact?session_id=not-created&name=legacy.txt", nil)
	rec = httptest.NewRecorder()
	h.ArtifactHandler(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "legacy" {
		t.Fatalf("legacy local artifact = %d %q, want 200 legacy", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/chat/artifacts?session_id=not-created", nil)
	rec = httptest.NewRecorder()
	h.ArtifactsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy local artifact list = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}
