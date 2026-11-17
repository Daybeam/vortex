package api

import (
	"crypto/subtle"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ── Upload static file serving ─────────────────────────────────────────────
//
// Uploaded files live under outputs/ and are referenced from chat/task
// messages as markdown images: ![name](/api/uploads/{id}/{name}). Because
// <img> tags cannot carry an Authorization header, these GET endpoints
// authenticate via an optional ?key= query parameter (constant-time
// compared), falling back to the Bearer header for programmatic clients.
// When no access key is configured (dev mode) access is open.

// servableImageTypes is the whitelist of extensions served by the upload
// static endpoints. It is intentionally limited to raster image types —
// only what markdown inline images need. SVG is deliberately excluded:
// served inline it would execute script in the app origin.
var servableImageTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
}

var (
	uploadAccessKeyMu sync.RWMutex
	uploadAccessKey   string
)

// SetUploadAccessKey configures the key required to read uploaded files
// via the static endpoints (?key= query param or Bearer header). An empty
// key means open access (dev mode). Call once at startup after resolving
// the admin key.
func SetUploadAccessKey(key string) {
	uploadAccessKeyMu.Lock()
	defer uploadAccessKeyMu.Unlock()
	uploadAccessKey = key
}

func getUploadAccessKey() string {
	uploadAccessKeyMu.RLock()
	defer uploadAccessKeyMu.RUnlock()
	return uploadAccessKey
}

// uploadAuthOK reports whether the request may read uploaded files.
// Dev mode (no key configured) allows everything.
func uploadAuthOK(r *http.Request) bool {
	key := getUploadAccessKey()
	if key == "" {
		return true
	}
	// Query param — the only channel an <img> tag can use.
	if q := r.URL.Query().Get("key"); q != "" &&
		subtle.ConstantTimeCompare([]byte(q), []byte(key)) == 1 {
		return true
	}
	// Bearer header fallback for fetch/XHR clients.
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

// ChatUploadServeHandler handles GET /api/uploads/{session_id}/{name} —
// serves chat-session upload files for inline markdown images.
func (h *APIHandler) ChatUploadServeHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/uploads/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || !validIDComponent(parts[0]) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	baseDir := filepath.Join("outputs", "chat_sessions", parts[0], "uploads")
	serveUploadFile(w, r, baseDir, parts[1])
}

// TaskUploadServeHandler handles GET /api/task-uploads/{upload_id}/{name} —
// serves task-submission upload files for inline markdown images.
func (h *APIHandler) TaskUploadServeHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/task-uploads/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || !validIDComponent(parts[0]) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	baseDir := filepath.Join("outputs", "task_uploads", parts[0], "uploads")
	serveUploadFile(w, r, baseDir, parts[1])
}

// serveUploadFile serves a single file from baseDir after validating the
// name component. Defense in depth: explicit rejection of traversal
// characters plus a prefix check on the resolved path (mirrors the
// audit S-H3 convention in StepResultHandler).
func serveUploadFile(w http.ResponseWriter, r *http.Request, baseDir, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !uploadAuthOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Name must be a single, clean path component.
	if name == "" || name != filepath.Base(name) ||
		strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}

	// Extension whitelist (raster images only).
	ext := strings.ToLower(filepath.Ext(name))
	contentType, ok := servableImageTypes[ext]
	if !ok {
		http.Error(w, "file type not servable", http.StatusForbidden)
		return
	}

	full := filepath.Join(baseDir, name)
	if !strings.HasPrefix(full, filepath.Clean(baseDir)+string(filepath.Separator)) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	f, err := os.Open(full)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", contentType)
	// private: the URL may embed the access key — never cache in shared caches.
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}
