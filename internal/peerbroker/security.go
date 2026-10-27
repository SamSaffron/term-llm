package peerbroker

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	idBytes         = 16
	capabilityBytes = 32
	maxJSONBody     = 16 << 10
)

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashCapability(s string) [32]byte {
	// Protocol capabilities are canonical 256-bit base64url values. Hash their
	// raw bytes so a host can persist and advertise only the hash. Retain raw
	// string hashing for operator-supplied test/admission values.
	if decoded, err := base64.RawURLEncoding.Strict().DecodeString(s); err == nil && len(decoded) == capabilityBytes && base64.RawURLEncoding.EncodeToString(decoded) == s {
		return sha256.Sum256(decoded)
	}
	return sha256.Sum256([]byte(s))
}

// GenerateCapability returns a 256-bit base64url capability suitable for
// host-generated invitation and device reconnect credentials.
func GenerateCapability() (string, error) { return randomToken(capabilityBytes) }

// HashCapability returns the canonical SHA-256 base64url hash sent when a host
// registers an invitation or device reconnect capability.
func HashCapability(capability string) string {
	sum := hashCapability(capability)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func decodeCapabilityHash(encoded string) ([32]byte, error) {
	var out [32]byte
	decoded, err := decodeToken(encoded, sha256.Size)
	if err != nil {
		return out, errors.New("invalid capability hash")
	}
	copy(out[:], decoded)
	return out, nil
}

func capabilityEqual(hash [32]byte, value string) bool {
	got := hashCapability(value)
	return subtle.ConstantTimeCompare(hash[:], got[:]) == 1
}

func bearer(r *http.Request) (string, bool) {
	v := r.Header.Get("Authorization")
	if len(v) > len("Bearer ") && len(v) <= len("Bearer ")+maxCredentialLen && strings.EqualFold(v[:len("Bearer ")], "Bearer ") {
		tok := v[len("Bearer "):]
		return tok, !strings.ContainsAny(tok, " \t\r\n")
	}
	return "", false
}

func hostIDForKey(key []byte) string {
	sum := sha256.Sum256(key)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func decodeToken(s string, size int) ([]byte, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != size || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, errors.New("invalid base64url value")
	}
	return b, nil
}

type tokenBucket struct {
	mu                  sync.Mutex
	rate, burst, tokens float64
	last                time.Time
	now                 func() time.Time
}

func newTokenBucket(rate float64, burst int, now func() time.Time) *tokenBucket {
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: now(), now: now}
}
func (b *tokenBucket) allow() bool { return b.allowN(1) }

func (b *tokenBucket) allowN(n float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed*b.rate)
	}
	b.last = now
	if n > b.tokens {
		return false
	}
	b.tokens -= n
	return true
}

func (b *tokenBucket) drain() {
	b.mu.Lock()
	b.tokens = 0
	b.last = b.now()
	b.mu.Unlock()
}
