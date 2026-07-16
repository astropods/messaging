package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store/files"
)

// lowSpaceStore is a real FSStore that reports a controllable amount of free
// space, so the capacity guard can be exercised deterministically without
// actually filling a disk.
type lowSpaceStore struct {
	*files.FSStore
	avail uint64
}

func (l *lowSpaceStore) Usage(context.Context) (files.Usage, error) {
	return files.Usage{
		TotalBytes:     1 << 30, // 1 GiB
		UsedBytes:      (1 << 30) - l.avail,
		AvailableBytes: l.avail,
	}, nil
}

func createFileReq(user, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/files", strings.NewReader(body))
	req.Header.Set("X-User-ID", user)
	return req
}

// A create whose size (plus reserved headroom) exceeds available space is
// rejected up front with 507, before any bytes are uploaded; a create that fits
// is admitted.
func TestCreateFile_RejectsWhenVolumeNearFull(t *testing.T) {
	fs, err := files.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}

	// Only 1 KiB free — a 1 MiB upload can't fit.
	nearFull := &Handlers{
		connManager:    NewConnectionManager(time.Second),
		sessionManager: NewHeaderSessionManager("X-User-ID", "", ""),
		fileStore:      &lowSpaceStore{FSStore: fs, avail: 1024},
	}
	w := httptest.NewRecorder()
	nearFull.HandleCreateFile(w, createFileReq("user-a", `{"name":"big.bin","size":1048576}`))
	if w.Code != http.StatusInsufficientStorage {
		t.Errorf("near-full create: expected 507, got %d (%s)", w.Code, w.Body.String())
	}

	// The same store with plenty of headroom admits the upload (real temp dir
	// has ample space).
	roomy := &Handlers{
		connManager:    NewConnectionManager(time.Second),
		sessionManager: NewHeaderSessionManager("X-User-ID", "", ""),
		fileStore:      fs,
	}
	w = httptest.NewRecorder()
	roomy.HandleCreateFile(w, createFileReq("user-a", `{"name":"ok.bin","size":1048576}`))
	if w.Code != http.StatusOK {
		t.Errorf("roomy create: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
}

// The usage endpoint reports real volume capacity for a filesystem store.
func TestFilesUsage_ReportsCapacity(t *testing.T) {
	fs, err := files.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	h := &Handlers{
		sessionManager: NewHeaderSessionManager("X-User-ID", "", ""),
		fileStore:      fs,
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/files/usage", nil)
	req.Header.Set("X-User-ID", "user-a")
	h.HandleFilesUsage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("usage: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp filesUsageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if !resp.Available || resp.TotalBytes == 0 {
		t.Errorf("expected available usage with nonzero total, got %+v", resp)
	}
	if resp.PercentUsed < 0 || resp.PercentUsed > 100 {
		t.Errorf("percent_used out of range: %v", resp.PercentUsed)
	}
}
