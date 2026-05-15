package web

// Locks in the authz gate for every protected web handler. The "bypass"
// tests below would each fail on the original PR — the audio handlers and
// HandleAgentConfig either skipped the authz check or skipped session+authz
// entirely, leaving paths where a denied principal still got data or actions.
//
// Run with:
//
//	go test ./internal/adapter/web -run TestAuthz -v

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/authz"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// denyAuthorizer always denies. If a handler runs the authz gate, the
// response is 403 (per Handlers.authenticate). If the gate is bypassed,
// the request continues and we observe a different shape — the regression
// these tests pin down.
type denyAuthorizer struct{ calls int }

func (d *denyAuthorizer) Authorize(_ context.Context, _, _, _, _ string) (authz.Result, error) {
	d.calls++
	return authz.Result{}, nil
}

var _ authz.Authorizer = (*denyAuthorizer)(nil)

func newHandlersWithDenyAuthz(configStore *store.AgentConfigStore) (*Handlers, *denyAuthorizer) {
	cm := NewConnectionManager(30 * time.Second)
	az := &denyAuthorizer{}
	h := NewHandlers(cm, &NoopSessionManager{}, nil, configStore)
	h.SetAuthorizer(az)
	return h, az
}

// --- Bypass: GET /api/agent/config ---------------------------------------
//
// Pre-fix: HandleAgentConfig had no authn or authz, so the system prompt
// and tool list leaked to any caller. Pinned here because the per-handler
// authenticate() pattern can silently miss new endpoints — this test makes
// the regression loud.

func TestAuthz_AgentConfig_DeniedPrincipal_LeaksSystemPrompt(t *testing.T) {
	configStore := store.NewAgentConfigStore()
	configStore.Set(&pb.AgentConfig{
		SystemPrompt: "TOP-SECRET-SYSTEM-PROMPT",
		Tools:        []*pb.AgentToolConfig{{Name: "secret_tool", Type: "other"}},
	})
	h, az := newHandlersWithDenyAuthz(configStore)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	w := httptest.NewRecorder()
	h.HandleAgentConfig(w, req)

	if az.calls == 0 {
		t.Errorf("HandleAgentConfig never called Authorizer.Allowed (bypassed authz)")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("HandleAgentConfig should return 403 for a denied principal; got %d body=%q",
			w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("TOP-SECRET-SYSTEM-PROMPT")) {
		t.Errorf("HandleAgentConfig leaked the system prompt to a denied principal: %s", w.Body.String())
	}
}

// HandleAgentConfig with no session must 401 — the gate is now session-first.
// Pre-fix it returned the prompt to anonymous requests.

func TestAuthz_AgentConfig_NoSession_LeaksSystemPrompt(t *testing.T) {
	configStore := store.NewAgentConfigStore()
	configStore.Set(&pb.AgentConfig{SystemPrompt: "TOP-SECRET-SYSTEM-PROMPT"})

	cm := NewConnectionManager(30 * time.Second)
	sm := NewHeaderSessionManager("X-User-ID", "", "") // header missing → ValidateRequest returns nil
	h := NewHandlers(cm, sm, nil, configStore)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	w := httptest.NewRecorder()
	h.HandleAgentConfig(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("HandleAgentConfig should return 401 with no session; got %d body=%q",
			w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("TOP-SECRET-SYSTEM-PROMPT")) {
		t.Errorf("HandleAgentConfig leaked the system prompt to an anonymous request: %s", w.Body.String())
	}
}

// --- Bypass: POST /api/conversations/{id}/audio --------------------------
//
// Pre-fix: session was validated but authz was skipped — a user who lost
// their grant for this deployment could keep streaming audio through it.

func TestAuthz_AudioUpload_DeniedPrincipal_NotForbidden(t *testing.T) {
	h, az := newHandlersWithDenyAuthz(nil)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("audio", "test.webm")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	_, _ = fw.Write([]byte("not real audio"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1/audio", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", "c1")
	w := httptest.NewRecorder()
	h.HandleAudioUpload(w, req)

	if az.calls == 0 {
		t.Errorf("HandleAudioUpload never called Authorizer.Allowed (bypassed authz); status=%d", w.Code)
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("HandleAudioUpload should return 403 for a denied principal; got %d body=%q",
			w.Code, w.Body.String())
	}
}

// --- Bypass: GET /api/conversations/{id}/audio (WebSocket) ---------------
//
// Pre-fix: same gap on the streaming path. The Upgrade fails on a non-WebSocket
// test request (typically returning 400), but pre-fix the handler had already
// passed the session check and skipped authz, so Allowed() was never called.

func TestAuthz_AudioStream_DeniedPrincipal_AuthzSkipped(t *testing.T) {
	h, az := newHandlersWithDenyAuthz(nil)

	req := httptest.NewRequest(http.MethodGet, "/api/conversations/c1/audio", nil)
	req.SetPathValue("id", "c1")
	w := httptest.NewRecorder()

	h.HandleAudioStream(w, req)

	if az.calls == 0 {
		t.Errorf("HandleAudioStream never called Authorizer.Allowed (bypassed authz); status=%d", w.Code)
	}
}

// --- Sanity: gated handlers really do 403 a denied principal -------------
//
// Contrast with the bypass tests above. HandleCreateConversation has always
// used authenticate(), so a denying Authorizer correctly produces a 403 with
// exactly one Allowed() call.

func TestAuthz_CreateConversation_DeniedPrincipal_Returns403(t *testing.T) {
	h, az := newHandlersWithDenyAuthz(nil)

	req := httptest.NewRequest(http.MethodPost, "/api/conversations", bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	h.HandleCreateConversation(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("HandleCreateConversation should 403 a denied principal; got %d body=%q",
			w.Code, w.Body.String())
	}
	if az.calls != 1 {
		t.Errorf("expected exactly one Allowed() call, got %d", az.calls)
	}
}
