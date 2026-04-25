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
	body, _ := json.Marshal(authorizeResponse{Allowed: allowed})
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
func TestAllowed_AnyoneFastPath_NoServerCall(t *testing.T) {
	a, stub := newTestAuthorizer(t, []string{"web"}, func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected server call: %s", r.URL)
		return nil, errors.New("should not call")
	})

	allowed, err := a.Allowed(context.Background(), "", "", "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("expected allowed=true via anyone fast path")
	}
	if stub.callCount.Load() != 0 {
		t.Errorf("expected 0 HTTP calls, got %d", stub.callCount.Load())
	}
}

// Adapter not in anyone_adapters → server call; result returned + cached.
func TestAllowed_ServerCall_AndCache(t *testing.T) {
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
		return okResp(true), nil
	})

	for i := 0; i < 3; i++ {
		allowed, err := a.Allowed(context.Background(), "user", "alice", "web")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !allowed {
			t.Errorf("call %d: expected allowed=true", i)
		}
	}
	if got := stub.callCount.Load(); got != 1 {
		t.Errorf("expected exactly 1 HTTP call (cache should hit on the rest), got %d", got)
	}
}

// Denied results are cached just like allowed ones.
func TestAllowed_DeniedIsCached(t *testing.T) {
	a, stub := newTestAuthorizer(t, nil, func(r *http.Request) (*http.Response, error) {
		return okResp(false), nil
	})

	for i := 0; i < 3; i++ {
		allowed, err := a.Allowed(context.Background(), "user", "bob", "web")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if allowed {
			t.Errorf("call %d: expected allowed=false", i)
		}
	}
	if got := stub.callCount.Load(); got != 1 {
		t.Errorf("expected 1 HTTP call (deny should be cached), got %d", got)
	}
}

// Transport errors fail closed (deny) and are NOT cached — the next call
// should retry rather than locking the principal out for the full TTL.
func TestAllowed_TransportError_FailClosedNotCached(t *testing.T) {
	a, stub := newTestAuthorizer(t, nil, func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})

	for i := 0; i < 2; i++ {
		allowed, err := a.Allowed(context.Background(), "user", "alice", "web")
		if err == nil {
			t.Errorf("call %d: expected error", i)
		}
		if allowed {
			t.Errorf("call %d: expected allowed=false on error", i)
		}
	}
	if got := stub.callCount.Load(); got != 2 {
		t.Errorf("expected each call to retry the server (no cache on error), got %d calls", got)
	}
}

// 5xx from the server is treated like a transport error: error returned,
// fail closed, no cache.
func TestAllowed_ServerError_FailClosed(t *testing.T) {
	a, _ := newTestAuthorizer(t, nil, func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(`{"error":"boom"}`)),
		}, nil
	})

	allowed, err := a.Allowed(context.Background(), "user", "alice", "web")
	if err == nil {
		t.Error("expected error on 500")
	}
	if allowed {
		t.Error("expected allowed=false on 500")
	}
}

// AllowAll / DenyAll do what they say.
func TestAllowAll_DenyAll(t *testing.T) {
	if ok, _ := AllowAll().Allowed(context.Background(), "", "", "web"); !ok {
		t.Error("AllowAll should allow")
	}
	if ok, _ := DenyAll().Allowed(context.Background(), "", "", "web"); ok {
		t.Error("DenyAll should deny")
	}
}

// NewAuthorizer wires real claims + client; rejects empty config.
func TestNewAuthorizer_RejectsMissingFields(t *testing.T) {
	if _, err := NewAuthorizer(Config{ServerURL: "http://x"}); err == nil {
		t.Error("expected error for missing IdentityToken")
	}
	if _, err := NewAuthorizer(Config{IdentityToken: jwt(`{"sub":"d"}`)}); err == nil {
		t.Error("expected error for missing ServerURL")
	}
	if _, err := NewAuthorizer(Config{IdentityToken: "garbage", ServerURL: "http://x"}); err == nil {
		t.Error("expected error for malformed token")
	}
}

func TestNewAuthorizer_HappyPath(t *testing.T) {
	tok := jwt(`{"sub":"dep-9","anyone_adapters":["web"]}`)
	a, err := NewAuthorizer(Config{IdentityToken: tok, ServerURL: "http://stub"})
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}
	// anyone_adapters=["web"] → web allowed without server call
	if ok, err := a.Allowed(context.Background(), "", "", "web"); !ok || err != nil {
		t.Errorf("expected allow on web; got ok=%v err=%v", ok, err)
	}
}
