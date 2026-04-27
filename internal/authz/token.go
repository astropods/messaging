// Package authz handles per-request authorization for the messaging container.
//
// The flow follows docs/01-spec/agent-authorization-spec.md (in the parent
// repo): astro-server signs an HS256 JWT (the "identity token") at deploy
// time and injects it as ASTRO_AUTHZ_TOKEN. The messaging container:
//
//  1. Decodes the token at startup to read its anyone_adapters claim. Any
//     request on an adapter listed there is allowed locally without calling
//     back to the server.
//  2. For every other request, calls /deployments/authorize on astro-server
//     using the raw token as a Bearer credential. The server validates the
//     signature, resolves the principal, and returns {allowed: bool}.
//  3. Caches the boolean per (deployment_id, identity_type, identity_id,
//     adapter) for a short TTL so a chatty session doesn't hammer the server.
//
// The container does NOT verify the token's signature itself — it has no
// access to the signing secret. The token's payload is trusted because:
//   - The token was placed in the container's env by the K8s spec applier,
//     i.e. by astro-server itself, over a trusted channel.
//   - The server-side authorize endpoint is the authoritative check, so a
//     misbehaving or stale anyone_adapters claim widens nothing — at worst
//     the container makes redundant authorize calls.
package authz

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// TokenClaims mirrors the subset of the deploy token's claims this package
// reads. Other fields (iss, exp, etc.) are ignored — the server enforces them.
type TokenClaims struct {
	Subject        string   `json:"sub"`
	Issuer         string   `json:"iss"`
	AnyoneAdapters []string `json:"anyone_adapters"`
}

// DecodeToken parses an HS256 JWT and returns its claims without verifying
// the signature. Use only on the messaging side; the server is the trusted
// authority. Returns an error for structural problems (wrong number of
// segments, malformed base64, unparseable JSON).
func DecodeToken(raw string) (*TokenClaims, error) {
	if raw == "" {
		return nil, errors.New("empty token")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("token: expected 3 segments, got %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Tolerate tokens encoded with padding (some signers add it).
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("token payload base64: %w", err)
		}
	}
	var c TokenClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("token payload json: %w", err)
	}
	if c.Subject == "" {
		return nil, errors.New("token missing sub claim")
	}
	return &c, nil
}
