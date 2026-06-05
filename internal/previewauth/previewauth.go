// Package previewauth provides the bearer-token primitives used by the CP's
// preview-URL gate. Both the create/rotate handlers in internal/api and the
// proxy enforcement in internal/proxy share this code so the token format,
// hash algorithm, and header parsing stay in sync.
package previewauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// GenerateToken returns a 32-byte (256-bit) random token, URL-safe base64
// encoded without padding (~43 chars). The entropy is sufficient that we
// store only a plain SHA-256 hex; PBKDF2/Argon2 would buy nothing here
// because the input is already incompressible.
func GenerateToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// SHA256Hex returns the lowercase hex SHA-256 digest of s.
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ExtractToken pulls the bearer token from one of two accepted headers.
// Returns "" when neither is present or the Authorization header uses a
// non-Bearer scheme. Tokens-in-querystring are deliberately not supported —
// they leak into logs and browser history.
func ExtractToken(r *http.Request) string {
	if x := r.Header.Get("X-OC-Preview-Token"); x != "" {
		return strings.TrimSpace(x)
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	if len(auth) < 7 || !strings.EqualFold(auth[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[7:])
}

// ConstantTimeEqualString wraps subtle.ConstantTimeCompare for two strings.
// Used by the preview-URL gate so a timing oracle can't distinguish "wrong
// by one character" from "wrong by all characters" on the hash compare.
func ConstantTimeEqualString(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ProcessRequest validates a create / rotate request and produces the
// plaintext token that will be returned to the caller plus the hash +
// scheme to persist. status > 0 indicates a 4xx that should be returned
// directly. The plaintext is the only value the caller ever sees.
//
//   - scheme defaults to "bearer"; any other value is a 400
//   - token == "" or "auto"        → server generates a 256-bit token
//   - token == "<≥16-char string>" → bring-your-own; echoed back to caller
//   - token == "<<16-char string>" → 400
func ProcessRequest(scheme, token string) (plaintext, hash, normalizedScheme string, status int, err error) {
	if scheme == "" {
		scheme = "bearer"
	}
	if scheme != "bearer" {
		return "", "", "", http.StatusBadRequest, fmt.Errorf("previewAuth.scheme must be %q (got %q)", "bearer", scheme)
	}
	switch {
	case token == "" || token == "auto":
		t, gErr := GenerateToken()
		if gErr != nil {
			return "", "", "", http.StatusInternalServerError, gErr
		}
		plaintext = t
	case len(token) >= 16:
		plaintext = token
	default:
		return "", "", "", http.StatusBadRequest, fmt.Errorf("previewAuth.token must be omitted, \"auto\", or a string of at least 16 characters")
	}
	return plaintext, SHA256Hex(plaintext), scheme, 0, nil
}
