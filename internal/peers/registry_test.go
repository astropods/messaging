package peers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// tokenWithIssuer builds an unsigned JWT-shaped token. DecodeToken never
// verifies the signature, so a placeholder segment is enough.
func tokenWithIssuer(t *testing.T, issuer string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"sub": "dep-1", "iss": issuer})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256"}`)) + "." + enc(payload) + ".sig"
}

type stubClient struct {
	calls    int
	lastReq  *http.Request
	handler  func(call int) (*http.Response, error)
	fallback func() (*http.Response, error)
}

func (c *stubClient) Do(req *http.Request) (*http.Response, error) {
	c.calls++
	c.lastReq = req
	if c.handler != nil {
		return c.handler(c.calls)
	}
	return c.fallback()
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func peersBody(names ...string) string {
	items := make([]string, 0, len(names))
	for _, n := range names {
		items = append(items, `{"deployment_id":"dep-`+n+`","agent_name":"`+n+`","url":"http://`+n+`:8100"}`)
	}
	return `{"peers":[` + strings.Join(items, ",") + `]}`
}

// newTestRegistry wires a Registry against a stub transport with a controllable
// clock, so TTL behavior is tested without sleeping.
func newTestRegistry(t *testing.T, stub *stubClient) (*Registry, *time.Time) {
	t.Helper()
	r, err := NewFromToken(tokenWithIssuer(t, "https://astro.example.com"))
	if err != nil {
		t.Fatalf("NewFromToken: %v", err)
	}
	clock := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	r.httpClient = stub
	r.now = func() time.Time { return clock }
	return r, &clock
}

func TestListFetchesPeersAndPresentsTheDeployToken(t *testing.T) {
	stub := &stubClient{fallback: func() (*http.Response, error) {
		return jsonResponse(http.StatusOK, peersBody("billing", "support")), nil
	}}
	r, _ := newTestRegistry(t, stub)

	got, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 2 || got[0].AgentName != "billing" {
		t.Fatalf("peers = %+v, want the two agents the server returned", got)
	}
	if url := stub.lastReq.URL.String(); url != "https://astro.example.com/api/v1/deployments/a2a/peers" {
		t.Errorf("request URL = %q, want the peers endpoint on the token's iss host", url)
	}
	if auth := stub.lastReq.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ey") {
		t.Errorf("Authorization = %q, want the raw deploy token as a Bearer credential", auth)
	}
}

func TestListServesTheCacheWithinTheTTL(t *testing.T) {
	stub := &stubClient{fallback: func() (*http.Response, error) {
		return jsonResponse(http.StatusOK, peersBody("billing")), nil
	}}
	r, clock := newTestRegistry(t, stub)

	if _, err := r.List(context.Background()); err != nil {
		t.Fatalf("first List: %v", err)
	}
	*clock = clock.Add(DefaultCacheTTL - time.Second)
	if _, err := r.List(context.Background()); err != nil {
		t.Fatalf("second List: %v", err)
	}

	if stub.calls != 1 {
		t.Errorf("made %d requests, want 1: a chatty agent must not re-ask the server per call", stub.calls)
	}
}

func TestListRefetchesOnceTheTTLExpires(t *testing.T) {
	stub := &stubClient{handler: func(call int) (*http.Response, error) {
		if call == 1 {
			return jsonResponse(http.StatusOK, peersBody("billing")), nil
		}
		return jsonResponse(http.StatusOK, peersBody("billing", "support")), nil
	}}
	r, clock := newTestRegistry(t, stub)

	if _, err := r.List(context.Background()); err != nil {
		t.Fatalf("first List: %v", err)
	}
	*clock = clock.Add(DefaultCacheTTL + time.Second)
	got, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("second List: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("got %d peers, want 2: an agent deployed after the first fetch must become visible", len(got))
	}
}

func TestListServesTheStaleListWhenARefreshFails(t *testing.T) {
	stub := &stubClient{handler: func(call int) (*http.Response, error) {
		if call == 1 {
			return jsonResponse(http.StatusOK, peersBody("billing")), nil
		}
		return nil, errors.New("connection refused")
	}}
	r, clock := newTestRegistry(t, stub)

	if _, err := r.List(context.Background()); err != nil {
		t.Fatalf("first List: %v", err)
	}
	*clock = clock.Add(DefaultCacheTTL + time.Second)
	got, err := r.List(context.Background())

	if err != nil {
		t.Fatalf("List returned %v, want the stale list: an agent mid-conversation must not lose its peers to a server hiccup", err)
	}
	if len(got) != 1 || got[0].AgentName != "billing" {
		t.Errorf("peers = %+v, want the previously cached list", got)
	}
}

func TestListErrorsWhenTheFirstFetchFails(t *testing.T) {
	stub := &stubClient{fallback: func() (*http.Response, error) {
		return nil, errors.New("connection refused")
	}}
	r, _ := newTestRegistry(t, stub)

	if _, err := r.List(context.Background()); err == nil {
		t.Fatal("want an error with nothing cached to fall back on, so the caller knows discovery is unavailable")
	}
}

func TestListSurfacesARejectionFromTheServer(t *testing.T) {
	stub := &stubClient{fallback: func() (*http.Response, error) {
		return jsonResponse(http.StatusForbidden, `{"error":"a2a is not enabled for this deployment"}`), nil
	}}
	r, _ := newTestRegistry(t, stub)

	_, err := r.List(context.Background())
	if err == nil {
		t.Fatal("want an error on 403")
	}
	if !strings.Contains(err.Error(), "a2a is not enabled") {
		t.Errorf("error = %v, want the server's reason included so the operator can act on it", err)
	}
}

func TestListReturnsAnEmptySliceForAnAccountWithNoPeers(t *testing.T) {
	stub := &stubClient{fallback: func() (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"peers":[]}`), nil
	}}
	r, _ := newTestRegistry(t, stub)

	got, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Error("want a non-nil empty slice so a caller can range without a guard")
	}
	if len(got) != 0 {
		t.Errorf("got %d peers, want 0", len(got))
	}
}

func TestInvalidateForcesTheNextListToRefetch(t *testing.T) {
	stub := &stubClient{fallback: func() (*http.Response, error) {
		return jsonResponse(http.StatusOK, peersBody("billing")), nil
	}}
	r, _ := newTestRegistry(t, stub)

	if _, err := r.List(context.Background()); err != nil {
		t.Fatalf("first List: %v", err)
	}
	r.Invalidate()
	if _, err := r.List(context.Background()); err != nil {
		t.Fatalf("second List: %v", err)
	}

	if stub.calls != 2 {
		t.Errorf("made %d requests, want 2: a caller that just failed to reach a peer needs a way to refresh", stub.calls)
	}
}

func TestNewFromTokenRejectsATokenWithNoIssuer(t *testing.T) {
	if _, err := NewFromToken(tokenWithIssuer(t, "")); err == nil {
		t.Fatal("want an error: iss is the only source of astro-server's URL, so there is nothing to call without it")
	}
}

func TestPeerNamePrefersTheDisplayName(t *testing.T) {
	if got := (Peer{AgentName: "billing-bot", DisplayName: "Billing"}).Name(); got != "Billing" {
		t.Errorf("Name() = %q, want the display name", got)
	}
	if got := (Peer{AgentName: "billing-bot"}).Name(); got != "billing-bot" {
		t.Errorf("Name() = %q, want the agent name when no display name is set", got)
	}
}

func TestFindResolvesAPeerByAnyOfItsNames(t *testing.T) {
	stub := &stubClient{fallback: func() (*http.Response, error) {
		return jsonResponse(http.StatusOK,
			`{"peers":[{"deployment_id":"dep-abc","agent_name":"billing-bot","display_name":"Billing Bot","url":"http://billing:8100"}]}`), nil
	}}
	r, _ := newTestRegistry(t, stub)

	for _, needle := range []string{"dep-abc", "billing-bot", "Billing Bot", "BILLING-BOT", "billing bot"} {
		peer, err := r.Find(context.Background(), needle)
		if err != nil {
			t.Errorf("Find(%q): %v — a model writes whichever name it saw", needle, err)
			continue
		}
		if peer.DeploymentID != "dep-abc" {
			t.Errorf("Find(%q) resolved to %q, want dep-abc", needle, peer.DeploymentID)
		}
	}
}

func TestFindNamesTheAvailableAgentsWhenTheNameIsWrong(t *testing.T) {
	stub := &stubClient{fallback: func() (*http.Response, error) {
		return jsonResponse(http.StatusOK, peersBody("billing", "support")), nil
	}}
	r, _ := newTestRegistry(t, stub)

	_, err := r.Find(context.Background(), "payroll")

	if err == nil {
		t.Fatal("want an error for an unknown agent")
	}
	for _, want := range []string{"billing", "support"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q: a model reads this to retry", err, want)
		}
	}
	if strings.Contains(err.Error(), "peers:") {
		t.Error("error carries the package prefix; this message is shown to a model verbatim")
	}
}

func TestFindSaysSoWhenTheOrganizationHasNoOtherAgents(t *testing.T) {
	stub := &stubClient{fallback: func() (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"peers":[]}`), nil
	}}
	r, _ := newTestRegistry(t, stub)

	_, err := r.Find(context.Background(), "billing")

	if err == nil {
		t.Fatal("want an error when there are no peers at all")
	}
	if !strings.Contains(err.Error(), "no other A2A agents") {
		t.Errorf("error = %v, want it to distinguish an empty organization from a wrong name", err)
	}
}

func TestFindRejectsAnEmptyName(t *testing.T) {
	stub := &stubClient{fallback: func() (*http.Response, error) {
		return jsonResponse(http.StatusOK, peersBody("billing")), nil
	}}
	r, _ := newTestRegistry(t, stub)

	if _, err := r.Find(context.Background(), "  "); err == nil {
		t.Fatal("want an error for a blank agent name")
	}
}
