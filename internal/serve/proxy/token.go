package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// tokenPlaintextPrefix is prepended to generated client tokens so they are
// recognizable in logs/config and distinct from the admin token.
const tokenPlaintextPrefix = "tlp_"

// tokenDisplayPrefixLen is how many leading characters of the plaintext token
// are stored for display (e.g. in admin listings). It is short enough to be
// non-sensitive but long enough to disambiguate.
const tokenDisplayPrefixLen = 12

// GenerateToken returns a new random client token in plaintext. The plaintext
// is shown to the operator exactly once; only its hash is persisted.
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate proxy token: %w", err)
	}
	return tokenPlaintextPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken returns the hex-encoded SHA-256 hash of a plaintext token. Tokens
// are high-entropy random secrets, so a fast cryptographic hash (rather than a
// slow password hash) is appropriate and keeps per-request auth cheap.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plaintext)))
	return hex.EncodeToString(sum[:])
}

// TokenDisplayPrefix returns a short, non-sensitive prefix of the plaintext
// token suitable for display in admin listings.
func TokenDisplayPrefix(plaintext string) string {
	plaintext = strings.TrimSpace(plaintext)
	if len(plaintext) <= tokenDisplayPrefixLen {
		return plaintext
	}
	return plaintext[:tokenDisplayPrefixLen]
}

// ConstantTimeEqual reports whether two secrets are equal without leaking timing
// information. Used for the admin token comparison.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
