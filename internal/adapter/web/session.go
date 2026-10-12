package web

import (
	"context"
	"net/http"
	"net/url"
)

// astro-server sets these on every proxied request; its proxy never forwards
// them from a client. The name is path-escaped, since a header value cannot
// hold every name.
const (
	HeaderUserID   = "X-Amzn-Oidc-Identity"
	HeaderUserName = "X-Astro-User-Name"
)

// Session represents an authenticated user session
type Session struct {
	UserID    string
	Username  string
	Email     string
	AvatarURL string
	Metadata  map[string]string
}

// SessionManager defines the interface for session authentication
type SessionManager interface {
	// ValidateRequest extracts and validates a session from an HTTP request
	// Returns nil if the request is not authenticated
	ValidateRequest(ctx context.Context, r *http.Request) (*Session, error)
}

// NoopSessionManager is a session manager that allows all requests
// Use for development or when authentication is handled elsewhere (e.g., API gateway)
type NoopSessionManager struct{}

// ValidateRequest always returns a default session
func (m *NoopSessionManager) ValidateRequest(ctx context.Context, r *http.Request) (*Session, error) {
	return &Session{
		UserID:   "anonymous",
		Username: "Anonymous User",
	}, nil
}

// HeaderSessionManager extracts session from HTTP headers
type HeaderSessionManager struct {
	UserIDHeader   string
	UsernameHeader string
	EmailHeader    string
}

// NewHeaderSessionManager creates a header-based session manager
func NewHeaderSessionManager(userIDHeader, usernameHeader, emailHeader string) *HeaderSessionManager {
	return &HeaderSessionManager{
		UserIDHeader:   userIDHeader,
		UsernameHeader: usernameHeader,
		EmailHeader:    emailHeader,
	}
}

// NewAstroServerSessionManager reads the identity astro-server's messaging
// proxy sets: the WorkOS user ID and the user's display name.
func NewAstroServerSessionManager() *HeaderSessionManager {
	return NewHeaderSessionManager(HeaderUserID, HeaderUserName, "")
}

// ValidateRequest extracts session from headers
func (m *HeaderSessionManager) ValidateRequest(ctx context.Context, r *http.Request) (*Session, error) {
	userID := r.Header.Get(m.UserIDHeader)
	if userID == "" {
		return nil, nil // No session
	}

	return &Session{
		UserID:   userID,
		Username: unescapeName(r.Header.Get(m.UsernameHeader)),
		Email:    r.Header.Get(m.EmailHeader),
	}, nil
}

// unescapeName drops a malformed name rather than fail the session over it.
func unescapeName(v string) string {
	name, err := url.PathUnescape(v)
	if err != nil {
		return ""
	}
	return name
}

// FixedSessionManager returns the same Session for every request. Used in
// local mode where there is no ingress to inject a per-request identity
// header — astro-server bakes the deploying account's owner user ID into
// the messaging pod env via AUTH_TEST_USER_ID and we surface it here so
// authz checks behave as if that user is signed in.
type FixedSessionManager struct {
	Session Session
}

// NewFixedSessionManager creates a session manager that always returns the
// given session.
func NewFixedSessionManager(s Session) *FixedSessionManager {
	return &FixedSessionManager{Session: s}
}

// ValidateRequest returns the fixed session regardless of request contents.
func (m *FixedSessionManager) ValidateRequest(_ context.Context, _ *http.Request) (*Session, error) {
	sess := m.Session
	return &sess, nil
}

// BearerTokenSessionManager validates bearer tokens
type BearerTokenSessionManager struct {
	// ValidateToken is a function that validates a token and returns session info
	ValidateToken func(ctx context.Context, token string) (*Session, error)
}

// NewBearerTokenSessionManager creates a bearer token session manager
func NewBearerTokenSessionManager(validateFn func(ctx context.Context, token string) (*Session, error)) *BearerTokenSessionManager {
	return &BearerTokenSessionManager{
		ValidateToken: validateFn,
	}
}

// ValidateRequest extracts and validates bearer token
func (m *BearerTokenSessionManager) ValidateRequest(ctx context.Context, r *http.Request) (*Session, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, nil
	}

	// Extract bearer token
	const prefix = "Bearer "
	if len(auth) < len(prefix) || auth[:len(prefix)] != prefix {
		return nil, nil
	}

	token := auth[len(prefix):]
	return m.ValidateToken(ctx, token)
}
