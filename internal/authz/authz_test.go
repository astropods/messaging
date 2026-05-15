package authz

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var _ = io.Discard // keep import (used by older Go before slog.DiscardHandler)

// testLogger discards everything — keeps test output clean.
func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// fakeHTTPClient stubs httpClient. Each Do call advances callCount and
// returns whatever fn returns.
type fakeHTTPClient struct {
	callCount atomic.Int32
	fn        func(req *http.Request) (*http.Response, error)
}

func (f *fakeHTTPClient) Do(req *http.Request) (*http.Response, error) {
	f.callCount.Add(1)
	return f.fn(req)
}

func okResp(allowed bool) *http.Response {
	return okRespWithUser(allowed, "")
}

func okRespWithUser(allowed bool, userID string) *http.Response {
	body, _ := json.Marshal(authorizeResponse{Allowed: allowed, UserID: userID})
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Header:     http.Header{},
	}
}

// newTestAuthorizer wires a realAuthorizer with a stub HTTP client so we
// can test the Authorizer flow without standing up a server.
func newTestAuthorizer(t *testing.T, anyoneAdapters []string, fn func(*http.Request) (*http.Response, error)) (*realAuthorizer, *fakeHTTPClient) {
	t.Helper()
	stub := &fakeHTTPClient{fn: fn}
	a := &realAuthorizer{
		deploymentID:   "dep-1",
		anyoneAdapters: anyoneAdapters,
		client: &authorizeClient{
			httpClient: stub,
			serverURL:  "http://stub",
			token:      "stub-token",
		},
		cache:  newResultCache(60 * time.Second),
		logger: testLogger(),
	}
	return a, stub
}

// Adapter listed in anyone_adapters → no server call, allowed immediately,
// no identity required.
func TestAuthorize_AnyoneFastPath_NoServerCall(t *testing.T) {
	a, stub := newTestAuthorizer(t, []string{"web"}, func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected server call: %s", r.URL)
		return nil, errors.New("should not call")
	})

	res, err := a.Authorize(context.Background(), "", "", "web", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Allowed {
		t.Error("expected allowed=true via anyone fast path")
	}
	if res.UserID != "" {
		t.Errorf("anyone-bypass must not resolve a user_id, got %q", res.UserID)
	}
	if stub.callCount.Load() != 0 {
		t.Errorf("expected 0 HTTP calls, got %d", stub.callCount.Load())
	}
}

// Adapter not in anyone_adapters → server call; result returned + cached.
func TestAuthorize_ServerCall_AndCache(t *testing.T) {
	a, stub := newTestAuthorizer(t, nil, func(r *http.Request) (*http.Response, error) {
		// Verify request shape.
		if got := r.Header.Get("Authorization"); got != "Bearer stub-token" {
			t.Errorf("Authorization header: got %q, want Bearer stub-token", got)
		}
		if got := r.URL.Query().Get("identity_type"); got != "user" {
			t.Errorf("identity_type: got %q", got)
		}
		if got := r.URL.Query().Get("identity_id"); got != "alice" {
			t.Errorf("identity_id: got %q", got)
		}
		if got := r.URL.Query().Get("adapter"); got != "web" {
			t.Errorf("adapter: got %q", got)
		}
		return okRespWithUser(true, "alice"), nil
	})

	for i := 0; i < 3; i++ {
		res, err := a.Authorize(context.Background(), "user", "alice", "web", "")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !res.Allowed {
			t.Errorf("call %d: expected allowed=true", i)
		}
		if res.UserID != "alice" {
			t.Errorf("call %d: expected userID=alice, got %q", i, res.UserID)
		}
	}
	if got := stub.callCount.Load(); got != 1 {
		t.Errorf("expected exactly 1 HTTP call (cache should hit on the rest), got %d", got)
	}
}

// Denied results are cached just like allowed ones.
func TestAuthorize_DeniedIsCached(t *testing.T) {
	a, stub := newTestAuthorizer(t, nil, func(r *http.Request) (*http.Response, error) {
		return okResp(false), nil
	})

	for i := 0; i < 3; i++ {
		res, err := a.Authorize(context.Background(), "user", "bob", "web", "")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if res.Allowed {
			t.Errorf("call %d: expected allowed=false", i)
		}
	}
	if got := stub.callCount.Load(); got != 1 {
		t.Errorf("expected 1 HTTP call (deny should be cached), got %d", got)
	}
}

// Server-resolved WorkOS user_id for slack identities round-trips through
// the client and the cache, so callers can rewrite msg.User.Id without an
// extra round-trip on subsequent messages.
func TestAuthorize_SlackResolvedUserIDFlowsThroughCache(t *testing.T) {
	a, stub := newTestAuthorizer(t, nil, func(r *http.Request) (*http.Response, error) {
		return okRespWithUser(true, "user_workos_42"), nil
	})

	for i := 0; i < 3; i++ {
		res, err := a.Authorize(context.Background(), "slack", "U01", "slack", "T1")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !res.Allowed || res.UserID != "user_workos_42" {
			t.Errorf("call %d: expected allowed=true userID=user_workos_42; got %+v", i, res)
		}
	}
	if got := stub.callCount.Load(); got != 1 {
		t.Errorf("expected 1 HTTP call; cache must replay user_id, got %d", got)
	}
}

// Transport errors fail closed (deny) and are NOT cached — the next call
// should retry rather than locking the principal out for the full TTL.
func TestAuthorize_TransportError_FailClosedNotCached(t *testing.T) {
	a, stub := newTestAuthorizer(t, nil, func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})

	for i := 0; i < 2; i++ {
		res, err := a.Authorize(context.Background(), "user", "alice", "web", "")
		if err == nil {
			t.Errorf("call %d: expected error", i)
		}
		if res.Allowed {
			t.Errorf("call %d: expected allowed=false on error", i)
		}
	}
	if got := stub.callCount.Load(); got != 2 {
		t.Errorf("expected each call to retry the server (no cache on error), got %d calls", got)
	}
}

// 5xx from the server is treated like a transport error: error returned,
// fail closed, no cache.
func TestAuthorize_ServerError_FailClosed(t *testing.T) {
	a, _ := newTestAuthorizer(t, nil, func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(`{"error":"boom"}`)),
		}, nil
	})

	res, err := a.Authorize(context.Background(), "user", "alice", "web", "")
	if err == nil {
		t.Error("expected error on 500")
	}
	if res.Allowed {
		t.Error("expected allowed=false on 500")
	}
}

// identity_scope is sent as a query param when non-empty and is part of
// the cache key — same identityID in two scopes must miss separately.
func TestAuthorize_IdentityScopeSentAndScopedInCache(t *testing.T) {
	var seenScope string
	a, stub := newTestAuthorizer(t, nil, func(r *http.Request) (*http.Response, error) {
		seenScope = r.URL.Query().Get("identity_scope")
		return okResp(true), nil
	})

	if _, err := a.Authorize(context.Background(), "slack", "U01", "slack", "T1"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if seenScope != "T1" {
		t.Errorf("identity_scope: got %q, want T1", seenScope)
	}

	// Same identity, different scope → cache must miss; the server gets a
	// second call with the new scope.
	if _, err := a.Authorize(context.Background(), "slack", "U01", "slack", "T2"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if seenScope != "T2" {
		t.Errorf("identity_scope on second call: got %q, want T2", seenScope)
	}
	if got := stub.callCount.Load(); got != 2 {
		t.Errorf("expected 2 HTTP calls (different scopes mustn't share a cache slot), got %d", got)
	}
}

// AllowAll / DenyAll do what they say.
func TestAllowAll_DenyAll(t *testing.T) {
	res, _ := AllowAll().Authorize(context.Background(), "user", "alice", "web", "")
	if !res.Allowed {
		t.Error("AllowAll should allow")
	}
	if res.UserID != "alice" {
		t.Errorf("AllowAll should echo identityID as userID, got %q", res.UserID)
	}
	res, _ = DenyAll().Authorize(context.Background(), "", "", "web", "")
	if res.Allowed {
		t.Error("DenyAll should deny")
	}
}

// NewAuthorizer wires real claims + client; rejects empty config.
func TestNewAuthorizer_RejectsMissingFields(t *testing.T) {
	if _, err := NewAuthorizer(Config{}); err == nil {
		t.Error("expected error for missing IdentityToken")
	}
	if _, err := NewAuthorizer(Config{IdentityToken: "garbage"}); err == nil {
		t.Error("expected error for malformed token")
	}
	// Token without iss claim — server URL can't be derived.
	if _, err := NewAuthorizer(Config{IdentityToken: jwt(`{"sub":"dep-1"}`)}); err == nil {
		t.Error("expected error for token missing iss")
	}
}

func TestNewAuthorizer_HappyPath(t *testing.T) {
	tok := jwt(`{"sub":"dep-9","iss":"https://astropods.com","anyone_adapters":["web"]}`)
	a, err := NewAuthorizer(Config{IdentityToken: tok})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}
	// anyone_adapters=["web"] → web allowed without server call
	res, err := a.Authorize(context.Background(), "", "", "web", "")
	if err != nil || !res.Allowed {
		t.Errorf("expected allow on web; got %+v err=%v", res, err)
	}
}
