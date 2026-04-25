package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// Identity types accepted by the authorize endpoint.
const (
	IdentityTypeUser  = "user"
	IdentityTypeSlack = "slack"

	AdapterWeb   = "web"
	AdapterSlack = "slack"
)

// DefaultCacheTTL is the cache window for boolean authorize results.
// Tuned to keep a chatty session cheap while letting grant changes propagate
// within ~a minute.
const DefaultCacheTTL = 60 * time.Second

// DefaultRequestTimeout caps each authorize round-trip. On timeout we
// fail-closed (deny) so a slow server can't accidentally widen access.
const DefaultRequestTimeout = 5 * time.Second

// Authorizer is the entry point used by adapters to check whether a request
// should be allowed.
type Authorizer interface {
	// Allowed returns true if the principal may use the deployment via the
	// named adapter. identityType / identityID may be empty for anonymous
	// requests; the server's anyone short-circuit may still allow them.
	Allowed(ctx context.Context, identityType, identityID, adapter string) (bool, error)
}

// Config configures a real Authorizer.
type Config struct {
	// IdentityToken is the raw ASTRO_AUTHZ_TOKEN env var value (HS256 JWT).
	// Required. When empty, NewAuthorizer returns an error — adapters should
	// short-circuit to AllowAll() in dev mode rather than constructing this.
	IdentityToken string
	// ServerURL is the base URL of astro-server (e.g. "http://astro-server:8080").
	ServerURL string
	// CacheTTL controls how long boolean results are cached. Zero → DefaultCacheTTL.
	CacheTTL time.Duration
	// RequestTimeout caps each authorize round-trip. Zero → DefaultRequestTimeout.
	RequestTimeout time.Duration
}

// realAuthorizer is the production implementation: anyone-fast-path from the
// token's claim, then a cached server callback.
type realAuthorizer struct {
	deploymentID    string
	anyoneAdapters  []string
	client          *authorizeClient
	cache           *resultCache
	logger          *slog.Logger
}

// NewAuthorizer parses the deploy token, sets up the HTTP client + cache,
// and returns a ready-to-use Authorizer. It does NOT verify the token's
// signature (the server does on every request).
func NewAuthorizer(cfg Config) (Authorizer, error) {
	if cfg.IdentityToken == "" {
		return nil, errors.New("authz: IdentityToken required")
	}
	if cfg.ServerURL == "" {
		return nil, errors.New("authz: ServerURL required")
	}
	claims, err := DecodeToken(cfg.IdentityToken)
	if err != nil {
		return nil, fmt.Errorf("authz: decode identity token: %w", err)
	}

	cacheTTL := cfg.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = DefaultCacheTTL
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}

	logger := slog.With(
		"component", "authz",
		"deployment_id", claims.Subject,
	)
	logger.Info("authorizer initialized",
		"server_url", cfg.ServerURL,
		"anyone_adapters", claims.AnyoneAdapters,
		"cache_ttl", cacheTTL,
	)

	return &realAuthorizer{
		deploymentID:   claims.Subject,
		anyoneAdapters: claims.AnyoneAdapters,
		client:         newAuthorizeClient(cfg.ServerURL, cfg.IdentityToken, timeout),
		cache:          newResultCache(cacheTTL),
		logger:         logger,
	}, nil
}

// Allowed implements Authorizer.
func (a *realAuthorizer) Allowed(ctx context.Context, identityType, identityID, adapter string) (bool, error) {
	// Fast path: adapter is publicly granted via the token's anyone_adapters
	// claim. No server round-trip, no cache lookup, no identity required.
	if slices.Contains(a.anyoneAdapters, adapter) {
		return true, nil
	}

	key := cacheKey{identityType: identityType, identityID: identityID, adapter: adapter}
	if allowed, ok := a.cache.get(key); ok {
		return allowed, nil
	}

	allowed, err := a.client.authorize(ctx, identityType, identityID, adapter)
	if err != nil {
		// Fail closed on transport/server errors; do not cache so we retry on
		// the next request rather than denying for the full TTL.
		a.logger.Warn("authorize call failed",
			"identity_type", identityType,
			"adapter", adapter,
			"error", err,
		)
		return false, err
	}
	a.cache.put(key, allowed)
	if !allowed {
		a.logger.Info("authorize denied",
			"identity_type", identityType,
			"adapter", adapter,
		)
	}
	return allowed, nil
}

// AllowAll returns an Authorizer that lets every request through. Used in
// dev mode (no ASTRO_AUTHZ_TOKEN configured) so local development doesn't
// require running astro-server.
func AllowAll() Authorizer { return allowAll{} }

type allowAll struct{}

func (allowAll) Allowed(_ context.Context, _ , _ , _ string) (bool, error) { return true, nil }

// DenyAll returns an Authorizer that denies every request. Used as the
// safe failure mode in production when configuration is missing — fail
// closed rather than open.
func DenyAll() Authorizer { return denyAll{} }

type denyAll struct{}

func (denyAll) Allowed(_ context.Context, _ , _ , _ string) (bool, error) { return false, nil }
