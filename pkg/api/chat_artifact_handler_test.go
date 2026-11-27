package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/tools"
)

// newArtifactTestHandler builds a ChatHandler whose OutputBase contains one
// legitimate artifact, plus a "secret" file in OutputBase's PARENT directory —
// the escape target for the traversal regression tests below.
func newArtifactTestHandler(t *testing.T) *ChatHandler {
	t.Helper()
	base := filepath.Join(t.TempDir(), "outputs")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatalf("mkdir outputs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "artifact.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	// secret.txt lives OUTSIDE base (in its parent). Reaching it = traversal.
	if err := os.WriteFile(filepath.Join(filepath.Dir(base), "secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	return &ChatHandler{Harness: &chat.ChatHarness{OutputBase: base}}
}

// TestArtifactHandler_ServesFileWithinBase is the happy path: a clean name that
// resolves inside OutputBase is served.
func TestArtifactHandler_ServesFileWithinBase(t *testing.T) {
	h := newArtifactTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/chat/artifact?name=artifact.txt", nil)
	rec := httptest.NewRecorder()
	h.ArtifactHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for in-base file, got %d", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Errorf("expected body %q, got %q", "hello", rec.Body.String())
	}
}

// TestArtifactHandler_RejectsNameTraversal: a `name` containing `..` must not
// escape OutputBase (pre-existing guard).
func TestArtifactHandler_RejectsNameTraversal(t *testing.T) {
	h := newArtifactTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/chat/artifact?name=../secret.txt", nil)
	rec := httptest.NewRecorder()
	h.ArtifactHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for traversal name, got %d", rec.Code)
	}
	if rec.Body.String() == "secret" {
		t.Fatal("traversal leaked file content outside base")
	}
}

// TestArtifactHandler_RejectsAbsName: an OS-absolute `name` is rejected.
// Note: a POSIX-style "/etc/passwd" is not absolute on Windows (filepath.IsAbs
// is false there), so it resolves inside base and 404s — also safe. This test
// uses a genuinely absolute path so the 400 branch is exercised on every OS.
func TestArtifactHandler_RejectsAbsName(t *testing.T) {
	h := newArtifactTestHandler(t)
	abs := filepath.Join(filepath.Dir(h.Harness.OutputBase), "secret.txt")
	req := httptest.NewRequest(http.MethodGet,
		"/api/chat/artifact?name="+url.QueryEscape(abs), nil)
	rec := httptest.NewRecorder()
	h.ArtifactHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for absolute name, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "secret" {
		t.Fatal("absolute name leaked file content outside base")
	}
}

// TestArtifactHandler_RejectsSessionIDTraversal is the SEC-1 (2026-10-23)
// regression test. `session_id` is joined directly into the path when the
// first candidate misses — and was never validated. Before the fix, this
// request resolved to <base-parent>/secret.txt and served it (200 + "secret").
// The path-prefix guard now rejects it with 400.
func TestArtifactHandler_RejectsSessionIDTraversal(t *testing.T) {
	h := newArtifactTestHandler(t)
	// name misses the first candidate → session_id branch is taken, and
	// "../.." climbs out of OutputBase to its parent, where secret.txt lives.
	req := httptest.NewRequest(http.MethodGet,
		"/api/chat/artifact?name=secret.txt&session_id=../..", nil)
	rec := httptest.NewRecorder()
	h.ArtifactHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for session_id traversal, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "secret" {
		t.Fatal("SEC-1: session_id traversal leaked file content outside base")
	}
}

// Regression: docs/completed/2026-11-27/OWNER_ISOLATION_SCHEDULE_SESSION_INBOX.md — artifact
// authorization must cover the directory actually read, not merely a supplied
// session_id. Before the fix, omitting session_id exposed global output and
// chat/<other-session>/ files; supplying one's own session_id did not help.
func TestArtifactOwnerIsolation_BindsReadsAndListingsToSession(t *testing.T) {
	base := filepath.Join(t.TempDir(), "outputs")
	h := &ChatHandler{
		App:     &tools.App{OwnerIsolation: true, Tier: tools.TierPublic},
		Harness: &chat.ChatHarness{OutputBase: base},
		Store:   chat.NewChatSessionStore(t.TempDir(), nil),
	}
	for id, owner := range map[string]string{"alice-session": "alice", "victim-session": "victim"} {
		if _, err := h.Store.GetOrCreateOwned(id, owner, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		"global.txt":                          "global",
		"chat/alice-session/own.txt":          "alice",
		"chat/victim-session/secret.txt":      "victim",
		"chat/alice-session/nested/note.txt": "alice nested",
	} {
		full := filepath.Join(base, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.WithValue(context.Background(), tools.AuthOwnerKey, "alice")
	ctx = context.WithValue(ctx, tools.AuthTierKey, tools.TierPublic)

	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"owned file", "session_id=alice-session&name=own.txt", http.StatusOK},
		{"owned nested file", "session_id=alice-session&name=nested/note.txt", http.StatusOK},
		{"global without session", "name=global.txt", http.StatusNotFound},
		{"foreign through omitted session", "name=chat/victim-session/secret.txt", http.StatusNotFound},
		{"foreign through own session", "session_id=alice-session&name=chat/victim-session/secret.txt", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ArtifactHandler(rec, httptest.NewRequest(http.MethodGet, "/api/chat/artifact?"+tc.query, nil).WithContext(ctx))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK && (rec.Body.String() == "global" || rec.Body.String() == "victim") {
				t.Fatalf("foreign artifact leaked: %q", rec.Body.String())
			}
		})
	}

	rec := httptest.NewRecorder()
	h.ArtifactsHandler(rec, httptest.NewRequest(http.MethodGet, "/api/chat/artifacts?session_id=alice-session", nil).WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("owned listing status = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "global.txt") || strings.Contains(body, "victim-session") || !strings.Contains(body, "own.txt") {
		t.Fatalf("owned listing escaped session scope: %s", body)
	}
	rec = httptest.NewRecorder()
	h.ArtifactsHandler(rec, httptest.NewRequest(http.MethodGet, "/api/chat/artifacts", nil).WithContext(ctx))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("global strict-mode listing status = %d, want 404", rec.Code)
	}

	adminCtx := context.WithValue(context.Background(), tools.AuthOwnerKey, "admin")
	adminCtx = context.WithValue(adminCtx, tools.AuthTierKey, tools.TierAdmin)
	rec = httptest.NewRecorder()
	h.ArtifactHandler(rec, httptest.NewRequest(http.MethodGet, "/api/chat/artifact?name=global.txt", nil).WithContext(adminCtx))
	if rec.Code != http.StatusOK || rec.Body.String() != "global" {
		t.Fatalf("admin positive control = %d %q", rec.Code, rec.Body.String())
	}
}
