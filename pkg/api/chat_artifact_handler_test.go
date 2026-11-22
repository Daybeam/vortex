package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/pkg/chat"
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
