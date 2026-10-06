package mesh

import (
	"errors"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func grantExpiry(grant string) (time.Time, error) {
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
