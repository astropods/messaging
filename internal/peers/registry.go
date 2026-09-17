// Package peers discovers the other A2A-enabled agents in the deployment's own
// account by asking astro-server, and caches the answer.
//
// The deploy token is the credential, exactly as in the authz package: its iss
// claim carries astro-server's base URL, so nothing else needs configuring, and
// the server resolves the account from the token's deployment rather than
// trusting a claim about it.
package peers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/astropods/messaging/internal/authz"
)

// DefaultCacheTTL is how long a peer list is reused. Matches
// authz.DefaultCacheTTL: the same server, the same tolerance for a newly
// deployed agent taking up to a minute to become visible.
const DefaultCacheTTL = 60 * time.Second

const defaultTimeout = 5 * time.Second

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Peer is one A2A-reachable agent in the same account.
type Peer struct {
	DeploymentID string `json:"deployment_id"`
	AgentName    string `json:"agent_name"`
	DisplayName  string `json:"display_name,omitempty"`
	URL          string `json:"url"`
}

// Name is the label to show a caller: the display name when the deployment has
// one, otherwise the agent name.
func (p Peer) Name() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.AgentName
}

type peersResponse struct {
	Peers []Peer `json:"peers"`
}

// Registry fetches and caches the account's A2A peer list.
type Registry struct {
	httpClient httpClient
	serverURL  string
	token      string
	ttl        time.Duration
	now        func() time.Time

	mu        sync.Mutex
	cached    []Peer
	fetched   bool
	expiresAt time.Time
}

// NewFromToken builds a Registry from the deploy token in ASTRO_AUTHZ_TOKEN.
func NewFromToken(token string) (*Registry, error) {
	claims, err := authz.DecodeToken(token)
	if err != nil {
		return nil, fmt.Errorf("peers: decode token: %w", err)
	}
	if strings.TrimSpace(claims.Issuer) == "" {
		return nil, errors.New("peers: token missing iss claim")
	}
	return &Registry{
		httpClient: &http.Client{Timeout: defaultTimeout},
		serverURL:  strings.TrimRight(claims.Issuer, "/"),
		token:      token,
		ttl:        DefaultCacheTTL,
		now:        time.Now,
	}, nil
}

// List returns the account's A2A peers, from cache when the entry is still
// fresh.
//
// A failed refresh returns the stale list rather than an error when one was
// ever fetched: an agent mid-conversation should not lose the peer it is
// talking to because astro-server hiccuped. The error only surfaces when
// there is nothing cached to fall back on.
func (r *Registry) List(ctx context.Context) ([]Peer, error) {
	r.mu.Lock()
	cached, fresh, everFetched := r.cached, r.now().Before(r.expiresAt), r.fetched
	r.mu.Unlock()

	if everFetched && fresh {
		return cached, nil
	}

	fetched, err := r.fetch(ctx)
	if err != nil {
		if everFetched {
			slog.Warn("[A2A] Peer refresh failed, serving cached list", "err", err, "peers", len(cached))
			return cached, nil
		}
		return nil, err
	}

	r.mu.Lock()
	r.cached, r.fetched, r.expiresAt = fetched, true, r.now().Add(r.ttl)
	r.mu.Unlock()
	return fetched, nil
}

// Invalidate drops the cached list so the next List refetches. For a caller
// that just failed to reach a peer and suspects the list is stale.
func (r *Registry) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expiresAt = time.Time{}
}

func (r *Registry) fetch(ctx context.Context) ([]Peer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.serverURL+"/api/v1/deployments/a2a/peers", nil)
	if err != nil {
		return nil, fmt.Errorf("peers: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)

	res, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("peers: request: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("peers: server returned %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed peersResponse
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("peers: decode response: %w", err)
	}
	// Never nil, so a caller can range without a guard and an empty account
	// reads as "no peers" rather than "not loaded".
	if parsed.Peers == nil {
		parsed.Peers = []Peer{}
	}
	return parsed.Peers, nil
}
