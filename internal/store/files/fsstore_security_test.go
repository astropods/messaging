package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// An agent can drop arbitrary files into the shared files directory. A symlink
// there must never be adopted as a plain file or served as blob content — else
// "output.txt -> /etc/passwd" would let the files API return a file outside the
// store. ReadMeta uses Lstat and OpenBlob uses O_NOFOLLOW so the link is refused
// at both the metadata and content paths.
func TestReadMetaAndOpenBlobRejectSymlink(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()

	// A secret file OUTSIDE the store directory.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// Agent drops a symlink in the store pointing at the secret.
	link := filepath.Join(dir, "evil.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	if _, err := s.ReadMeta(ctx, "evil.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadMeta on symlink: expected ErrNotFound, got %v (must not adopt a symlink)", err)
	}
	if _, err := s.OpenBlob(ctx, "evil.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenBlob on symlink: expected ErrNotFound, got %v (must not serve a symlink target)", err)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, m := range list {
		if m.Name == "evil.txt" {
			t.Errorf("List surfaced a symlink entry: %+v", m)
		}
	}

	// A genuine sibling regular file is still adopted and served (regression).
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	if _, err := s.ReadMeta(ctx, "real.txt"); err != nil {
		t.Errorf("ReadMeta on real file: unexpected error %v", err)
	}
	rc, err := s.OpenBlob(ctx, "real.txt")
	if err != nil {
		t.Fatalf("OpenBlob on real file: %v", err)
	}
	_ = rc.Close()
}

// Ownership is immutable once a real user owns a file. AttributeOwner assigns an
// unowned (agent-written) file to a first owner, refuses to transfer it to a
// second, and is a no-op for the existing owner — the compare-and-set that stops
// a shared agent from moving one user's file into another user's conversation.
func TestAttributeOwnerImmutableCompareAndSet(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()

	// Agent-written plain file: unowned.
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	if m, err := s.ReadMeta(ctx, "out.txt"); err != nil || m.UploadedBy != agentOwner {
		t.Fatalf("adopted file: expected UploadedBy=%q, got %q (err=%v)", agentOwner, m.UploadedBy, err)
	}

	// First attribution wins.
	m, err := s.AttributeOwner(ctx, "out.txt", "user-b")
	if err != nil || m.UploadedBy != "user-b" {
		t.Fatalf("attribute to user-b: got %q err=%v", m.UploadedBy, err)
	}
	if got, _ := s.ReadMeta(ctx, "out.txt"); got.UploadedBy != "user-b" {
		t.Fatalf("attribution not persisted: got %q", got.UploadedBy)
	}

	// A second user cannot steal it.
	if _, err := s.AttributeOwner(ctx, "out.txt", "user-c"); !errors.Is(err, ErrOwnedByOther) {
		t.Errorf("attribute to user-c: expected ErrOwnedByOther, got %v", err)
	}
	if got, _ := s.ReadMeta(ctx, "out.txt"); got.UploadedBy != "user-b" {
		t.Errorf("ownership changed after refused transfer: got %q", got.UploadedBy)
	}

	// Re-attributing to the existing owner is a no-op success.
	if _, err := s.AttributeOwner(ctx, "out.txt", "user-b"); err != nil {
		t.Errorf("re-attribute to owner: expected nil, got %v", err)
	}
}
