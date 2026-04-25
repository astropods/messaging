package authz

import (
	"encoding/base64"
	"strings"
	"testing"
)

// jwt builds a fake unsigned-JWT-shaped string with the given JSON payload.
// The signature segment is a placeholder; DecodeToken must not look at it.
func jwt(payload string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return header + "." + body + ".sig"
}

func TestDecodeToken_Roundtrip(t *testing.T) {
	tok := jwt(`{"sub":"dep-1","iss":"astro-server","anyone_adapters":["web"]}`)
	c, err := DecodeToken(tok)
	if err != nil {
		t.Fatalf("DecodeToken: %v", err)
	}
	if c.Subject != "dep-1" {
		t.Errorf("Subject: got %q, want dep-1", c.Subject)
	}
	if c.Issuer != "astro-server" {
		t.Errorf("Issuer: got %q", c.Issuer)
	}
	if len(c.AnyoneAdapters) != 1 || c.AnyoneAdapters[0] != "web" {
		t.Errorf("AnyoneAdapters: got %v", c.AnyoneAdapters)
	}
}

func TestDecodeToken_EmptyAnyoneAdapters(t *testing.T) {
	tok := jwt(`{"sub":"dep-1","iss":"astro-server"}`)
	c, err := DecodeToken(tok)
	if err != nil {
		t.Fatalf("DecodeToken: %v", err)
	}
	if len(c.AnyoneAdapters) != 0 {
		t.Errorf("expected empty AnyoneAdapters, got %v", c.AnyoneAdapters)
	}
}

func TestDecodeToken_MissingSub(t *testing.T) {
	tok := jwt(`{"iss":"astro-server"}`)
	if _, err := DecodeToken(tok); err == nil {
		t.Fatal("expected error for missing sub")
	}
}

func TestDecodeToken_Empty(t *testing.T) {
	if _, err := DecodeToken(""); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestDecodeToken_WrongSegmentCount(t *testing.T) {
	if _, err := DecodeToken("a.b"); err == nil {
		t.Fatal("expected error for 2-segment token")
	}
	if _, err := DecodeToken(strings.Repeat("a.", 5) + "z"); err == nil {
		t.Fatal("expected error for 6-segment token")
	}
}

func TestDecodeToken_MalformedBase64(t *testing.T) {
	if _, err := DecodeToken("a.!!!.b"); err == nil {
		t.Fatal("expected error for non-base64 payload")
	}
}

func TestDecodeToken_MalformedJSON(t *testing.T) {
	tok := jwt(`{not-json`)
	if _, err := DecodeToken(tok); err == nil {
		t.Fatal("expected error for invalid JSON payload")
	}
}

// Real tokens from astro-server use base64.RawURLEncoding (no padding).
// Make sure we still accept padded encodings if some signer adds them.
func TestDecodeToken_PaddedBase64(t *testing.T) {
	header := base64.URLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	body := base64.URLEncoding.EncodeToString([]byte(`{"sub":"dep-1"}`))
	tok := header + "." + body + ".sig"
	c, err := DecodeToken(tok)
	if err != nil {
		t.Fatalf("DecodeToken: %v", err)
	}
	if c.Subject != "dep-1" {
		t.Errorf("Subject: got %q", c.Subject)
	}
}
