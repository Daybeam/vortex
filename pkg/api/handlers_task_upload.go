package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// maxTaskUploadRequestSize caps the whole multipart request body for
// POST /api/tasks/upload. Per-file limits are enforced by
// validateUploadFile (MaxUploadFileSize); this guards against a client
// declaring a small size but streaming far more bytes.
const maxTaskUploadRequestSize = 64 * 1024 * 1024

// maxIDComponentLen bounds client-supplied id components (upload_id) that
// end up inside filesystem paths.
const maxIDComponentLen = 64

// TaskUploadHandler handles POST /api/tasks/upload — accepts multipart file
// uploads that will be attached to a task submission. Files are stored under
// outputs/task_uploads/{upload_id}/uploads/ and later seeded into the task
// workspace via the input_dir mechanism (see seedTaskInputs).
//
// Form fields:
//   - upload_id: optional; a fresh uuid is generated when empty. Must match
//     [A-Za-z0-9_-] and be ≤ 64 chars (it becomes a path component).
//   - files: one or more file parts (same validation as chat uploads).
//
// Response: {ok, upload_id, files:[{name,size,path}]} where path is the
// InputDir-relative path ("uploads/{name}").
func (h *APIHandler) TaskUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxTaskUploadRequestSize)

	// Parse multipart form (max 32MB in memory, rest spills to disk)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "failed to parse multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}

	uploadID := strings.TrimSpace(r.FormValue("upload_id"))
	if uploadID == "" {
		uploadID = uuid.New().String()
	} else if !validIDComponent(uploadID) {
		http.Error(w, "invalid upload_id", http.StatusBadRequest)
		return
	}

	// Resolve the uploads directory: outputs/task_uploads/{upload_id}/uploads/
	uploadDir := filepath.Join("outputs", "task_uploads", uploadID, "uploads")
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
		// Validate filename and size (shared with chat uploads)
		safeName, err := validateUploadFile(header.Filename, header.Size)
		if err != nil {
			writeJSON(w, map[string]any{
				"ok":        false,
				"upload_id": uploadID,
				"error":     fmt.Sprintf("file %q rejected: %v", header.Filename, err),
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
			os.Remove(dstPath)
			http.Error(w, "failed to write file: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Run the optional content validator hook (same policy as chat)
		if v := getFileContentValidator(); v != nil {
			if err := v(dstPath); err != nil {
				os.Remove(dstPath) // clean up rejected file
				writeJSON(w, map[string]any{
					"ok":        false,
					"upload_id": uploadID,
					"error":     fmt.Sprintf("file %q content rejected: %v", safeName, err),
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
		"ok":        true,
		"upload_id": uploadID,
		"files":     results,
	})
}

// validIDComponent reports whether s is safe to use as a single path
// component: [A-Za-z0-9_-] only, non-empty, ≤ maxIDComponentLen chars.
// Used for client-supplied ids (upload_id) before they touch the filesystem.
func validIDComponent(s string) bool {
	if s == "" || len(s) > maxIDComponentLen {
		return false
	}
	for _, c := range s {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}
