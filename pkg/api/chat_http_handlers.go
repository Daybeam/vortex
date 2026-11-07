package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/store"
)

// mbStoreForHarness returns the MemoryBankStore when MemoryBankEnabled is
// true, nil otherwise. This gates ChatHarness injection + ReflectionEngine
// extraction via a single config toggle.

func writeSSE(w http.ResponseWriter, ev chat.ChatEvent) {
	fmt.Fprintf(w, "id: %d\n", ev.Seq)
	fmt.Fprintf(w, "event: %s\n", ev.Type)
	data, _ := json.Marshal(ev)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func parseSeq(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (h *ChatHandler) ArtifactsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	base := h.Harness.OutputBase
	if base == "" {
		base = "outputs"
	}
	sessionID := r.URL.Query().Get("session_id")

	type artifactEntry struct {
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		Modified int64  `json:"modified"`
		IsDir    bool   `json:"is_dir"`
	}

	var entries []artifactEntry

	scanDir := func(dir string) {
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			return
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, f := range files {
			info, err := f.Info()
			if err != nil {
				continue
			}
			entries = append(entries, artifactEntry{
				Name:     f.Name(),
				Size:     info.Size(),
				Modified: info.ModTime().Unix(),
				IsDir:    f.IsDir(),
			})
		}
	}

	scanDir(base)
	if sessionID != "" {
		scanDir(filepath.Join(base, "chat", sessionID))
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Modified > entries[j].Modified
	})

	writeJSON(w, map[string]any{"artifacts": entries})
}

func (h *ChatHandler) ArtifactHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" || strings.Contains(name, "..") || filepath.IsAbs(name) {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}
	base := h.Harness.OutputBase
	if base == "" {
		base = "outputs"
	}
	sessionID := r.URL.Query().Get("session_id")

	candidate := filepath.Join(base, filepath.Clean(name))
	if _, err := os.Stat(candidate); err != nil && sessionID != "" {
		candidate = filepath.Join(base, "chat", sessionID, filepath.Clean(name))
	}

	// audit SEC-1: the `name` check above does not cover `session_id`, which is
	// joined directly into the path when the first candidate misses. Guard the
	// final resolved path so neither input can escape `base` (same pattern as
	// handlers_upload_serve.go).
	if !strings.HasPrefix(candidate, filepath.Clean(base)+string(filepath.Separator)) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	data, err := os.ReadFile(candidate)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// HistoryHandler handles GET /api/chat/history?session_id=... — returns the
// message tree (active branch path) for a chat session so the frontend can
// restore past conversations on page reload or tab switch.
func (h *ChatHandler) HistoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeJSON(w, map[string]any{"messages": []any{}, "session_id": ""})
		return
	}
	session := h.Store.GetOrCreate(sessionID)
	msgs := session.SnapshotMessages()
	if msgs == nil {
		msgs = []chat.ChatMessage{}
	}
	// branch_points: messages on the active path that have more than one
	// child in the tree — i.e. points where the conversation was forked.
	// Computed on read (not persisted) so the UI can mark fork origins.
	var branchPoints []string
	for _, m := range msgs {
		if len(session.ListBranches(m.ID)) > 1 {
			branchPoints = append(branchPoints, m.ID)
		}
	}
	writeJSON(w, map[string]any{
		"session_id":    sessionID,
		"messages":      msgs,
		"branch_points": branchPoints,
	})
}

// SessionsHandler handles GET /api/chat/sessions — lists recent chat sessions
// so the frontend can show a session picker / history sidebar.
// Supports pagination via ?limit=N&offset=M query params.
func (h *ChatHandler) SessionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 20
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	sessions, err := h.Store.ListSessions(r.Context(), limit, offset)
	if err != nil {
		writeJSON(w, map[string]any{"sessions": []any{}, "error": err.Error()})
		return
	}
	if sessions == nil {
		sessions = []store.ChatSessionRow{}
	}
	writeJSON(w, map[string]any{"sessions": sessions, "has_more": len(sessions) == limit})
}

// UploadHandler handles POST /api/chat/upload — accepts multipart file uploads
// for a chat session and stores them in the session's uploads directory.
// Files are validated (size, extension, filename) before storage.
// The optional FileContentValidator hook is run after each file is written.
func (h *ChatHandler) UploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse multipart form (max 32MB in memory, rest spills to disk)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "failed to parse multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}

	sessionID := r.FormValue("session_id")
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	// Resolve the uploads directory: outputs/chat_sessions/{session_id}/uploads/
	uploadDir := filepath.Join("outputs", "chat_sessions", sessionID, "uploads")
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		http.Error(w, "failed to create upload directory: "+err.Error(), http.StatusInternalServerError)
		return
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "no files provided", http.StatusBadRequest)
		return
	}

	type uploadedFile struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		Path string `json:"path"`
	}
	var results []uploadedFile

	for _, header := range files {
		// Validate filename and size
		safeName, err := validateUploadFile(header.Filename, header.Size)
		if err != nil {
			writeJSON(w, map[string]any{
				"ok":         false,
				"session_id": sessionID,
				"error":      fmt.Sprintf("file %q rejected: %v", header.Filename, err),
			})
			return
		}

		// Open the uploaded file
		src, err := header.Open()
		if err != nil {
			http.Error(w, "failed to open uploaded file: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Write to the destination path
		dstPath := filepath.Join(uploadDir, safeName)
		dst, err := os.Create(dstPath)
		if err != nil {
			src.Close()
			http.Error(w, "failed to create file: "+err.Error(), http.StatusInternalServerError)
			return
		}

		written, err := io.Copy(dst, src)
		src.Close()
		dst.Close()
		if err != nil {
			http.Error(w, "failed to write file: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Run the optional content validator hook
		if v := getFileContentValidator(); v != nil {
			if err := v(dstPath); err != nil {
				os.Remove(dstPath) // clean up rejected file
				writeJSON(w, map[string]any{
					"ok":         false,
					"session_id": sessionID,
					"error":      fmt.Sprintf("file %q content rejected: %v", safeName, err),
				})
				return
			}
		}

		results = append(results, uploadedFile{
			Name: safeName,
			Size: written,
			Path: filepath.Join("uploads", safeName), // relative path for InputDir
		})
	}

	writeJSON(w, map[string]any{
		"ok":         true,
		"session_id": sessionID,
		"files":      results,
	})
}
