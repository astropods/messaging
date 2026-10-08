package roomgrant

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/astropods/messaging/internal/authz"
)

type Grant struct {
	RoomID    string
	Grant     string
	ExpiresAt time.Time
	APIURL    string
}

// Expiry reads exp without verifying the signature; astro-server verifies the grant.
func Expiry(grant string) (time.Time, error) {
	token, err := jwt.ParseSigned(grant, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		return time.Time{}, err
	}
	var claims jwt.Claims
	if err := token.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return time.Time{}, err
	}
	if claims.Expiry == nil {
		return time.Time{}, errors.New("grant has no exp")
	}
	return claims.Expiry.Time(), nil
}

func APIURL(identityToken string) (string, error) {
	claims, err := authz.DecodeToken(identityToken)
	if err != nil {
		return "", fmt.Errorf("decode ASTRO_AUTHZ_TOKEN: %w", err)
	}
	return strings.TrimSuffix(claims.Issuer, "/"), nil
}

func Live(roomID, grant, apiURL string, now time.Time) (Grant, bool) {
	if roomID == "" || grant == "" {
		return Grant{}, false
	}
	expires, err := Expiry(grant)
	if err != nil || !now.Before(expires) {
		return Grant{}, false
	}
	return Grant{RoomID: roomID, Grant: grant, ExpiresAt: expires, APIURL: apiURL}, true
}
