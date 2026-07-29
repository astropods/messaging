package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/store/files"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// The web client hides the composer's upload affordance unless agent/config
// reports capabilities.files=true. Uploads are only usable when the sidecar has
// a file store wired AND the agent declared it consumes attachments, so assert
// the full truth table — a storage-only sidecar (agent that never wires up the
// files API) must report false.
func TestHandleAgentConfig_ReportsFilesCapability(t *testing.T) {
	filesCap := func(storage, declared bool) bool {
		cs := store.NewAgentConfigStore()
		cs.Set(&pb.AgentConfig{SystemPrompt: "sp", SupportsFiles: declared})
		h := NewHandlers(NewConnectionManager(time.Second), &NoopSessionManager{}, nil, cs)
		if storage {
			fs, err := files.NewFSStore(t.TempDir())
			if err != nil {
				t.Fatalf("file store: %v", err)
			}
			h.fileStore = fs
		}
		req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
		w := httptest.NewRecorder()
		h.HandleAgentConfig(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("agent/config: got %d, body=%q", w.Code, w.Body.String())
		}
		var resp struct {
			Capabilities struct {
				Files bool `json:"files"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode agent/config: %v", err)
		}
		return resp.Capabilities.Files
	}

	cases := []struct {
		storage  bool
		declared bool
		want     bool
	}{
		{storage: true, declared: true, want: true},
		{storage: true, declared: false, want: false}, // storage wired but agent never wires up files
		{storage: false, declared: true, want: false}, // agent declares but there's nowhere to store uploads
		{storage: false, declared: false, want: false},
	}
	for _, tc := range cases {
		if got := filesCap(tc.storage, tc.declared); got != tc.want {
			t.Errorf("filesCap(storage=%v, declared=%v) = %v, want %v", tc.storage, tc.declared, got, tc.want)
		}
	}
}
