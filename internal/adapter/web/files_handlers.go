package web

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/astropods/messaging/internal/store/files"
	"github.com/google/uuid"
)

// Agent files API. These endpoints back per-deployment file upload/download from
// a persistent volume via the FileStore, served through the astro-server
// /files/* proxy. Files are addressed by an opaque, server-generated key so the
// same contract maps onto a filesystem today and presigned S3 later; the
// download and upload handlers already branch on the store's presign support, so
// swapping in an S3 store needs no client or contract change.

const (
	// filesMaxNameRunes caps the user-facing filename length.
	filesMaxNameRunes = 255
	// filesMaxUploadBytes bounds a single uploaded file. The default agent disk
	// is small (5Gi shared), so this is a coarse guard against a single file
	// filling it; raise alongside the volume size if needed.
	filesMaxUploadBytes = 100 << 20 // 100 MiB
	// filesStorageReserveBytes is headroom kept free on the shared volume when
	// admitting a new upload, so an upload can't consume the last bytes the chat
	// store and metadata sidecars need. Uploads that would eat into it are
	// rejected up front (507) rather than failing mid-write.
	filesStorageReserveBytes = 32 << 20 // 32 MiB
)

type uploadDescriptor struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
}

type createFileInput struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

type createFileResponse struct {
	Key    string           `json:"key"`
	File   files.FileMeta   `json:"file"`
	Upload uploadDescriptor `json:"upload"`
}

type listFilesResponse struct {
	Files []files.FileMeta `json:"files"`
}

// filesUsageResponse reports the backing volume's capacity so the client can
// warn as it fills. Available is false when the store can't report usage (an
// S3-backed store, or a platform without statfs) — the client then hides the
// capacity banner rather than showing a misleading 0%.
type filesUsageResponse struct {
	Available      bool    `json:"available"`
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	PercentUsed    float64 `json:"percent_used"`
}

// HandleCreateFile handles POST /api/files. It reserves an opaque key, persists
// the declared metadata, and returns an upload descriptor telling the client
// where to send the bytes: a presigned URL when the store supports it (S3), or a
// relative content path handled by HandlePutFileContent (filesystem).
func (h *Handlers) HandleCreateFile(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	if h.fileStore == nil {
		http.Error(w, "file storage is not enabled", http.StatusNotFound)
		return
	}

	var input createFileInput
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&input)
	}
	name, ok := sanitizeFileName(input.Name)
	if !ok {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}
	if input.Size < 0 || input.Size > filesMaxUploadBytes {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Reject before the upload starts if the shared volume can't fit the file
	// plus headroom, so the user gets a clear "storage full" instead of a
	// mid-stream write failure. A store that can't report usage (S3, or statfs
	// unavailable) skips the check; the write-time ENOSPC guard below is the
	// backstop.
	if usage, err := h.fileStore.Usage(r.Context()); err == nil && usage.TotalBytes > 0 {
		if uint64(input.Size)+filesStorageReserveBytes > usage.AvailableBytes {
			http.Error(w, "not enough storage available on the deployment volume", http.StatusInsufficientStorage)
			return
		}
	}

	meta := files.FileMeta{
		Key:         uuid.NewString(),
		Name:        name,
		Size:        input.Size,
		ContentType: normalizeContentType(input.ContentType),
		UpdatedAt:   time.Now().UTC(),
		UploadedBy:  session.UserID,
	}

	// Prefer a direct-to-store upload when the backend supports it (S3). The
	// filesystem store returns ErrUnsupported, so we persist metadata now and
	// hand back a relative content path for the server-received PUT.
	if target, err := h.fileStore.PresignPut(r.Context(), meta); err == nil {
		if err := h.fileStore.WriteMeta(r.Context(), meta); err != nil {
			slog.Error("[Web] files create: write meta failed", "err", err)
			http.Error(w, "failed to create file", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, createFileResponse{
			Key:  meta.Key,
			File: meta,
			Upload: uploadDescriptor{
				URL:     target.URL,
				Method:  methodOrDefault(target.Method, http.MethodPut),
				Headers: target.Headers,
			},
		})
		return
	} else if !errors.Is(err, files.ErrUnsupported) {
		slog.Error("[Web] files create: presign failed", "err", err)
		http.Error(w, "failed to create file", http.StatusInternalServerError)
		return
	}

	if err := h.fileStore.WriteMeta(r.Context(), meta); err != nil {
		slog.Error("[Web] files create: write meta failed", "err", err)
		http.Error(w, "failed to create file", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, createFileResponse{
		Key:  meta.Key,
		File: meta,
		// Relative to the files base URL (with trailing slash); the client
		// resolves it, so an absolute presigned URL later needs no client change.
		Upload: uploadDescriptor{URL: meta.Key + "/content", Method: http.MethodPut},
	})
}

// HandlePutFileContent handles PUT /api/files/{key}/content — the server-received
// upload path for stores without presigned uploads. It streams the request body
// into the blob and reconciles the recorded size.
func (h *Handlers) HandlePutFileContent(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	key, ok := parseFileKey(r.PathValue("key"))
	if !ok {
		http.Error(w, "invalid file key", http.StatusBadRequest)
		return
	}
	if h.fileStore == nil {
		http.Error(w, "file storage is not enabled", http.StatusNotFound)
		return
	}

	meta, err := h.fileStore.ReadMeta(r.Context(), key)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		slog.Error("[Web] files put: read meta failed", "err", err)
		http.Error(w, "failed to store file", http.StatusInternalServerError)
		return
	}
	if !ownsFile(session, meta) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	body := http.MaxBytesReader(w, r.Body, filesMaxUploadBytes)
	n, err := h.fileStore.WriteBlob(r.Context(), key, body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
			return
		}
		if errors.Is(err, syscall.ENOSPC) {
			// The volume filled mid-write (size under-declared, or a concurrent
			// writer raced us past the create-time check). Surface it as 507 so
			// the client shows "storage full", not a generic error.
			http.Error(w, "not enough storage available on the deployment volume", http.StatusInsufficientStorage)
			return
		}
		slog.Error("[Web] files put: write blob failed", "err", err)
		http.Error(w, "failed to store file", http.StatusInternalServerError)
		return
	}

	meta.Size = n
	meta.UpdatedAt = time.Now().UTC()
	if err := h.fileStore.WriteMeta(r.Context(), meta); err != nil {
		slog.Error("[Web] files put: update meta failed", "err", err)
		http.Error(w, "failed to store file", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// HandleGetFileContent handles GET /api/files/{key}/content. When the store
// supports presigned reads it redirects the client straight to the store (S3);
// otherwise it streams the bytes from the blob (filesystem). Both paths look
// identical to a redirect-following client.
func (h *Handlers) HandleGetFileContent(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	key, ok := parseFileKey(r.PathValue("key"))
	if !ok {
		http.Error(w, "invalid file key", http.StatusBadRequest)
		return
	}
	if h.fileStore == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	meta, err := h.fileStore.ReadMeta(r.Context(), key)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		slog.Error("[Web] files get content: read meta failed", "err", err)
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}

	if !ownsFile(session, meta) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	if url, err := h.fileStore.PresignGet(r.Context(), key); err == nil {
		// #nosec G710 -- url is a store-issued presigned URL, not user-controlled.
		http.Redirect(w, r, url, http.StatusFound)
		return
	} else if !errors.Is(err, files.ErrUnsupported) {
		slog.Error("[Web] files get content: presign failed", "err", err)
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}

	blob, err := h.fileStore.OpenBlob(r.Context(), key)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			http.Error(w, "file content not found", http.StatusNotFound)
			return
		}
		slog.Error("[Web] files get content: open blob failed", "err", err)
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	defer func() { _ = blob.Close() }()

	ct := meta.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", contentDisposition(meta.Name))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, blob); err != nil {
		slog.Debug("[Web] files get content: copy failed", "err", err)
	}
}

// HandleListFiles handles GET /api/files.
func (h *Handlers) HandleListFiles(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	if h.fileStore == nil {
		writeJSON(w, http.StatusOK, listFilesResponse{Files: []files.FileMeta{}})
		return
	}
	list, err := h.fileStore.List(r.Context())
	if err != nil {
		slog.Error("[Web] files list failed", "err", err)
		http.Error(w, "failed to list files", http.StatusInternalServerError)
		return
	}
	// Per-user scope: only surface the requester's own files (uploads and
	// agent-produced files attributed to them). Files uploaded by other members
	// of the same account are not listed here.
	owned := make([]files.FileMeta, 0, len(list))
	for _, m := range list {
		if ownsFile(session, m) {
			owned = append(owned, m)
		}
	}
	writeJSON(w, http.StatusOK, listFilesResponse{Files: owned})
}

// ownsFile reports whether the file belongs to the requesting user. File access
// is per-user: the uploader (or, for agent outputs, the conversation owner the
// file was attributed to) is the only principal allowed to read/write/delete it.
func ownsFile(session *Session, meta files.FileMeta) bool {
	return session != nil && meta.UploadedBy == session.UserID
}

// HandleFilesUsage handles GET /api/files/usage — capacity of the volume backing
// the file store, so the client can warn as it approaches full. Usage is
// volume-wide (not per-user): the whole shared PVC is the resource that fills,
// regardless of who owns which file, so any authenticated user sees the same
// numbers. Stores with no fixed capacity (S3) or platforms without statfs report
// Available=false and the client hides the banner.
func (h *Handlers) HandleFilesUsage(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	if h.fileStore == nil {
		writeJSON(w, http.StatusOK, filesUsageResponse{Available: false})
		return
	}
	u, err := h.fileStore.Usage(r.Context())
	if err != nil || u.TotalBytes == 0 {
		if err != nil && !errors.Is(err, files.ErrUnsupported) {
			slog.Warn("[Web] files usage failed", "err", err)
		}
		writeJSON(w, http.StatusOK, filesUsageResponse{Available: false})
		return
	}
	writeJSON(w, http.StatusOK, filesUsageResponse{
		Available:      true,
		TotalBytes:     u.TotalBytes,
		UsedBytes:      u.UsedBytes,
		AvailableBytes: u.AvailableBytes,
		PercentUsed:    float64(u.UsedBytes) / float64(u.TotalBytes) * 100,
	})
}

// HandleGetFile handles GET /api/files/{key} — metadata only.
func (h *Handlers) HandleGetFile(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	key, ok := parseFileKey(r.PathValue("key"))
	if !ok {
		http.Error(w, "invalid file key", http.StatusBadRequest)
		return
	}
	if h.fileStore == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	meta, err := h.fileStore.ReadMeta(r.Context(), key)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		slog.Error("[Web] files get failed", "err", err)
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	if !ownsFile(session, meta) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// HandleDeleteFile handles DELETE /api/files/{key}.
func (h *Handlers) HandleDeleteFile(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	key, ok := parseFileKey(r.PathValue("key"))
	if !ok {
		http.Error(w, "invalid file key", http.StatusBadRequest)
		return
	}
	if h.fileStore == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Only the owner may delete. Read metadata first so a non-owner can't remove
	// another user's file; a missing file is already a no-op (idempotent delete).
	meta, err := h.fileStore.ReadMeta(r.Context(), key)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		slog.Error("[Web] files delete: read meta failed", "err", err)
		http.Error(w, "failed to delete file", http.StatusInternalServerError)
		return
	}
	if !ownsFile(session, meta) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if err := h.fileStore.Delete(r.Context(), key); err != nil {
		slog.Error("[Web] files delete failed", "err", err)
		http.Error(w, "failed to delete file", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseFileKey validates a route file key. API-created files use an opaque UUID,
// but the store also surfaces plain files an agent drops into the files directory
// (keyed by their filename), so any single safe path segment is accepted: no
// path separators, no traversal, no control characters. filepath.Base in the
// store is a second guard against escaping the directory.
func parseFileKey(raw string) (string, bool) {
	if raw == "" || raw == "." || raw == ".." {
		return "", false
	}
	if utf8.RuneCountInString(raw) > filesMaxNameRunes {
		return "", false
	}
	for _, r := range raw {
		if r == '/' || r == '\\' || r == 0 || unicode.IsControl(r) {
			return "", false
		}
	}
	return raw, true
}

// sanitizeFileName trims and validates the user-facing name. It rejects empty,
// over-length, and control-character names but does not otherwise constrain the
// value — the name is metadata only and never used to build a storage path.
func sanitizeFileName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", false
	}
	if utf8.RuneCountInString(name) > filesMaxNameRunes {
		return "", false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return "", false
		}
	}
	return name, true
}

func normalizeContentType(ct string) string {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}

func methodOrDefault(method, fallback string) string {
	if strings.TrimSpace(method) == "" {
		return fallback
	}
	return method
}

// contentDisposition builds an attachment header, quoting the sanitized filename.
func contentDisposition(name string) string {
	// Backslashes and quotes would break the quoted-string; strip them (the name
	// is display-only, control chars already rejected on upload).
	safe := strings.NewReplacer("\"", "", "\\", "").Replace(name)
	if safe == "" {
		safe = "download"
	}
	return "attachment; filename=\"" + safe + "\""
}
