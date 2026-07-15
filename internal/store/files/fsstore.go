package files

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// File name suffixes for the on-disk artifacts of an API-managed file. The
// opaque key is the base name; the blob holds the bytes and the meta holds the
// JSON sidecar. ".tmp" is the in-flight suffix used for atomic writes.
const (
	blobSuffix = ".blob"
	metaSuffix = ".meta.json"
	tmpSuffix  = ".tmp"
)

// FSStore is a filesystem-backed FileStore over a single directory (the
// deployment's shared persistent volume, mounted into the sidecar). It holds two
// kinds of entries, both listable/downloadable/deletable through the same API:
//
//   - API-managed files: written by uploads as "<uuid>.blob" + "<uuid>.meta.json".
//   - Adopted plain files: any other regular file in the directory — e.g. a file
//     the agent itself drops into /data/files. These have no sidecar; their
//     metadata is synthesized from the filename and stat, and the filename is
//     their key.
//
// This is what lets an agent "hand a file back" simply by writing it into the
// shared files directory. It does not support presigned transfer.
type FSStore struct {
	dir string
}

// isReservedName reports whether a directory entry is an API-managed artifact
// (a blob or meta sidecar) or an in-flight temp file, and therefore must not be
// surfaced as an adopted plain file.
func isReservedName(name string) bool {
	return strings.HasSuffix(name, blobSuffix) ||
		strings.HasSuffix(name, metaSuffix) ||
		strings.HasSuffix(name, tmpSuffix)
}

// adoptedMeta synthesizes metadata for a plain file that has no sidecar. The
// filename is both the display name and the opaque key.
func adoptedMeta(name string, info os.FileInfo) FileMeta {
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return FileMeta{
		Key:         name,
		Name:        name,
		Size:        info.Size(),
		ContentType: ct,
		UpdatedAt:   info.ModTime().UTC(),
		UploadedBy:  "agent",
	}
}

// NewFSStore returns a store rooted at dir, creating dir if needed.
func NewFSStore(dir string) (*FSStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("files: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("files: create dir %q: %w", dir, err)
	}
	return &FSStore{dir: dir}, nil
}

// keyPath maps an opaque key to an on-disk path with the given suffix (managed
// artifacts). Keys are validated by the caller to be a safe, path-segment-free
// token, so filepath.Base is a defense-in-depth guard against traversal, not the
// primary check.
func (s *FSStore) keyPath(key, suffix string) string {
	return filepath.Join(s.dir, filepath.Base(key)+suffix)
}

// plainPath maps a key to the on-disk path of an adopted plain file (no suffix).
func (s *FSStore) plainPath(key string) string {
	return filepath.Join(s.dir, filepath.Base(key))
}

func (s *FSStore) WriteMeta(_ context.Context, meta FileMeta) error {
	if meta.Key == "" {
		return fmt.Errorf("files: WriteMeta requires a key")
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = time.Now().UTC()
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("files: marshal meta: %w", err)
	}
	// Write atomically: a partial meta file would break List and ReadMeta.
	tmp := s.keyPath(meta.Key, metaSuffix) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("files: write meta: %w", err)
	}
	if err := os.Rename(tmp, s.keyPath(meta.Key, metaSuffix)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("files: commit meta: %w", err)
	}
	return nil
}

func (s *FSStore) ReadMeta(_ context.Context, key string) (FileMeta, error) {
	// API-managed file: explicit sidecar.
	data, err := os.ReadFile(s.keyPath(key, metaSuffix))
	if err == nil {
		var meta FileMeta
		if uerr := json.Unmarshal(data, &meta); uerr != nil {
			return FileMeta{}, fmt.Errorf("files: unmarshal meta: %w", uerr)
		}
		return meta, nil
	}
	if !os.IsNotExist(err) {
		return FileMeta{}, fmt.Errorf("files: read meta: %w", err)
	}
	// Adopted plain file: a regular file with no sidecar (e.g. dropped by the
	// agent). Synthesize metadata from the filename + stat.
	base := filepath.Base(key)
	if info, statErr := os.Stat(s.plainPath(key)); statErr == nil {
		if info.Mode().IsRegular() && !isReservedName(base) {
			return adoptedMeta(base, info), nil
		}
	}
	return FileMeta{}, ErrNotFound
}

func (s *FSStore) List(ctx context.Context) ([]FileMeta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("files: list dir: %w", err)
	}

	// Pass 1: API-managed files (from their sidecars). Record their display names
	// so a plain file of the same name (e.g. an agent that wrote output next to an
	// upload) isn't listed a second time — the managed record is authoritative
	// (real key, uploader, size).
	out := make([]FileMeta, 0, len(entries))
	managedNames := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), metaSuffix) {
			continue
		}
		key := strings.TrimSuffix(e.Name(), metaSuffix)
		meta, err := s.ReadMeta(ctx, key)
		if err != nil {
			// A meta file that fails to parse is skipped rather than failing the
			// whole listing — one bad entry shouldn't hide the rest.
			continue
		}
		out = append(out, meta)
		managedNames[meta.Name] = true
	}

	// Pass 2: adopted plain files (e.g. dropped by the agent), skipping managed
	// artifacts and any name already surfaced as a managed file.
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if isReservedName(name) || managedNames[name] {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, adoptedMeta(name, info))
	}

	// Newest first for a stable, useful default order in the UI.
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (s *FSStore) WriteBlob(_ context.Context, key string, r io.Reader) (int64, error) {
	tmp := s.keyPath(key, blobSuffix) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("files: create blob: %w", err)
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return n, fmt.Errorf("files: write blob: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return n, fmt.Errorf("files: close blob: %w", closeErr)
	}
	if err := os.Rename(tmp, s.keyPath(key, blobSuffix)); err != nil {
		_ = os.Remove(tmp)
		return n, fmt.Errorf("files: commit blob: %w", err)
	}
	return n, nil
}

func (s *FSStore) OpenBlob(_ context.Context, key string) (io.ReadCloser, error) {
	// API-managed blob first.
	f, err := os.Open(s.keyPath(key, blobSuffix))
	if err == nil {
		return f, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("files: open blob: %w", err)
	}
	// Adopted plain file: the file itself is the blob.
	base := filepath.Base(key)
	if isReservedName(base) {
		return nil, ErrNotFound
	}
	pf, perr := os.Open(s.plainPath(key))
	if perr != nil {
		if os.IsNotExist(perr) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("files: open blob: %w", perr)
	}
	if info, serr := pf.Stat(); serr != nil || !info.Mode().IsRegular() {
		_ = pf.Close()
		return nil, ErrNotFound
	}
	return pf, nil
}

func (s *FSStore) Delete(_ context.Context, key string) error {
	// Remove every artifact a key could map to (managed blob + sidecar, or an
	// adopted plain file). Missing files are not an error, so delete is
	// idempotent regardless of which kind the key is.
	for _, p := range []string{
		s.keyPath(key, blobSuffix),
		s.keyPath(key, metaSuffix),
		s.plainPath(key),
	} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("files: delete %q: %w", filepath.Base(p), err)
		}
	}
	return nil
}

// PresignPut / PresignGet are unsupported on the filesystem store; the API falls
// back to server-mediated transfer.
func (s *FSStore) PresignPut(context.Context, FileMeta) (UploadTarget, error) {
	return UploadTarget{}, ErrUnsupported
}

func (s *FSStore) PresignGet(context.Context, string) (string, error) {
	return "", ErrUnsupported
}
