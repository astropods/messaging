package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
)

func TestHandleReady_ReportsWhetherASendWouldReachAnAgent(t *testing.T) {
	cases := []struct {
		name      string
		readiness adapter.AgentReadiness
		want      bool
	}{
		{"agent has not registered its stream", func() bool { return false }, false},
		{"agent stream registered", func() bool { return true }, true},
		{"sidecar cannot observe the stream", nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandlers(NewConnectionManager(time.Second), &NoopSessionManager{}, nil, nil)
			h.agentReadiness = tc.readiness

			w := httptest.NewRecorder()
			h.HandleReady(w, httptest.NewRequest(http.MethodGet, "/api/ready", nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; a client cannot distinguish a not-ready agent from a broken route", w.Code)
			}
			var resp struct {
				AgentConnected bool `json:"agent_connected"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v, body=%q", err, w.Body.String())
			}
			if resp.AgentConnected != tc.want {
				t.Errorf("agent_connected = %v, want %v", resp.AgentConnected, tc.want)
			}
		})
	}
}
