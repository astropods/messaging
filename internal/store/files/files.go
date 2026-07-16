// Package files is the storage backend for the agent files API (upload/download)
// exposed by the messaging sidecar's web adapter.
//
// Files are addressed by an opaque, server-generated key — never by a filesystem
// path — so the same key maps 1:1 onto a filesystem name today and an S3 object
// key later. The FileStore interface is the single swap point: the filesystem
// implementation (FSStore) backs a per-deployment persistent volume; an S3
// implementation can be dropped in by satisfying the same interface (including
// the presign methods) with no change to the HTTP contract or the client.
package files

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrUnsupported is returned by store operations a backend does not implement —
// notably the presign methods on the filesystem store, which has no notion of a
// direct-transfer URL. Callers treat it as "fall back to server-mediated
// transfer" rather than a hard failure.
var ErrUnsupported = errors.New("files: operation not supported by this store")

// ErrNotFound is returned when a key has no metadata or no blob.
var ErrNotFound = errors.New("files: not found")

// ErrOwnedByOther is returned by AttributeOwner when the file already has a real
// user owner different from the requested one. Ownership is immutable once set,
// so a shared agent can't transfer one user's file into another's conversation.
var ErrOwnedByOther = errors.New("files: already owned by another user")

// File lifecycle states, tracked in FileMeta.Status. A managed upload is created
// StatusReserved (metadata only, no bytes), promoted to StatusReady once the blob
// is committed. Only ready files are listable, downloadable, or attachable, so an
// abandoned or in-flight upload never surfaces as a broken download chip. The
// empty string is treated as ready for backward compatibility and for adopted
// plain files, whose bytes exist the moment they're seen.
const (
	StatusReserved  = "reserved"
	StatusUploading = "uploading"
	StatusReady     = "ready"
)

// FileMeta describes one stored file. Key is the opaque id used across the API
// and as the storage key; Name is the user-facing filename (never used to build
// a storage path).
type FileMeta struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	UpdatedAt   time.Time `json:"updated_at"`
	// UploadedBy records the opaque user id that owns the file. File access is
	// per-user: this is the access-control key (see web.ownsFile), not merely
	// audit metadata. Agent-written plain files are unowned ("agent") until
	// attributed to a conversation owner via AttributeOwner. Once a real user
	// owns a file the value is immutable.
	UploadedBy string `json:"uploaded_by,omitempty"`
	// Status is the lifecycle state (see the Status* constants). Empty means
	// ready (adopted plain files and legacy metadata predating this field).
	Status string `json:"status,omitempty"`
}

// Ready reports whether the file's bytes are committed and it may be listed,
// downloaded, or attached. Empty status is ready (adopted/legacy).
func (m FileMeta) Ready() bool {
	return m.Status == "" || m.Status == StatusReady
}

// UploadTarget is a direct-upload descriptor for presign-capable stores (S3):
// the client PUTs bytes straight to URL with the given method and headers,
// bypassing the server. Filesystem stores return ErrUnsupported from PresignPut
// and the API falls back to a server-received PUT.
type UploadTarget struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Usage reports byte-level capacity of the volume backing the store. For the
// filesystem store this is the whole underlying volume (on the shared /data PVC
// that means chat DB + files + agent outputs together) — the real capacity the
// files feature competes for. Presign-capable stores (S3) have no fixed
// capacity and return ErrUnsupported.
type Usage struct {
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// FileStore persists file metadata and blobs. Metadata and blob are stored and
// read separately so the two-step upload (reserve key + metadata, then stream
// bytes) and the presigned S3 path share one interface.
type FileStore interface {
	// WriteMeta creates or updates metadata for meta.Key.
	WriteMeta(ctx context.Context, meta FileMeta) error
	// ReadMeta returns metadata for key, or ErrNotFound.
	ReadMeta(ctx context.Context, key string) (FileMeta, error)
	// AttributeOwner atomically assigns owner to key iff the file is currently
	// unowned (agent-written, no real user owner), and returns the resulting
	// metadata. If key is already owned by owner it is a no-op success; if it is
	// owned by a different real user it returns ErrOwnedByOther without changing
	// anything. The compare-and-set is what prevents two concurrent agent
	// responses from reassigning the same file across users.
	AttributeOwner(ctx context.Context, key, owner string) (FileMeta, error)
	// List returns metadata for every stored file (newest first is not
	// guaranteed; callers sort as needed).
	List(ctx context.Context) ([]FileMeta, error)
	// WriteBlob streams r into key's blob and returns the number of bytes
	// written. It does not update metadata size — the caller reconciles.
	WriteBlob(ctx context.Context, key string, r io.Reader) (int64, error)
	// OpenBlob opens key's blob for reading, or ErrNotFound. The caller closes it.
	OpenBlob(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes both blob and metadata for key. Missing is not an error.
	Delete(ctx context.Context, key string) error
	// PresignPut returns a direct-upload target, or ErrUnsupported.
	PresignPut(ctx context.Context, meta FileMeta) (UploadTarget, error)
	// PresignGet returns a direct-download URL, or ErrUnsupported.
	PresignGet(ctx context.Context, key string) (string, error)
	// Usage reports capacity of the backing volume, or ErrUnsupported for stores
	// with no fixed capacity (S3).
	Usage(ctx context.Context) (Usage, error)
}
