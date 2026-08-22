package passkeyauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	HostGrantLifetime    = 10 * time.Minute
	GrantSessionLifetime = 5 * time.Minute
	CeremonyLifetime     = 5 * time.Minute
	MaxGrantSessions     = 8
	MaxCeremonies        = 512
	MaxCeremoniesPerPeer = 16
)

type GrantKind string

const (
	GrantBootstrap GrantKind = "bootstrap"
	GrantRecovery  GrantKind = "recovery"
)

type CeremonyKind string

const (
	CeremonyLogin         CeremonyKind = "login"
	CeremonyReauth        CeremonyKind = "reauth"
	CeremonyBootstrap     CeremonyKind = "bootstrap"
	CeremonyRecovery      CeremonyKind = "recovery"
	CeremonyAddCredential CeremonyKind = "add_credential"
)

var (
	ErrInvalidGrant    = errors.New("invalid or expired host grant")
	ErrGrantConsumed   = errors.New("host grant already consumed")
	ErrCapacity        = errors.New("authentication capacity reached")
	ErrInvalidCeremony = errors.New("invalid or expired ceremony")
)

type GrantSession struct {
	ID, Token string
	Kind      GrantKind
	ExpiresAt time.Time
}
type grantRecord struct {
	id       string
	hash     [32]byte
	kind     GrantKind
	expires  time.Time
	consumed bool
}
type Grants struct {
	mu           sync.Mutex
	now          func() time.Time
	random       io.Reader
	rootHash     [32]byte
	configured   bool
	rootExpires  time.Time
	rootConsumed bool
	kind         GrantKind
	records      map[[32]byte]*grantRecord
}

func NewGrants(kind GrantKind, secret []byte, now func() time.Time, random io.Reader) (*Grants, error) {
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	g := &Grants{now: now, random: random, kind: kind, records: map[[32]byte]*grantRecord{}}
	if len(secret) == 0 {
		return g, nil
	}
	if err := ValidateHostSecret(secret); err != nil {
		return nil, err
	}
	g.rootHash = sha256.Sum256(canonicalSecret(secret))
	g.configured = true
	g.rootExpires = now().UTC().Add(HostGrantLifetime)
	return g, nil
}
func ValidateHostSecret(secret []byte) error {
	n := 0
	for _, b := range secret {
		if !unicode.IsSpace(rune(b)) {
			n++
		}
	}
	if n < 20 {
		return fmt.Errorf("passkey host secret must contain at least 20 non-whitespace bytes")
	}
	return nil
}
func canonicalSecret(secret []byte) []byte { return []byte(strings.TrimSpace(string(secret))) }
func GenerateBootstrapSecret(r io.Reader) ([]byte, string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, 20)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, "", err
	}
	raw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	parts := make([]string, 0, 8)
	for len(raw) > 0 {
		n := 4
		if len(raw) < n {
			n = len(raw)
		}
		parts = append(parts, raw[:n])
		raw = raw[n:]
	}
	display := strings.Join(parts, "-")
	return []byte(display), display, nil
}
func (g *Grants) Enabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.configured && !g.rootConsumed && g.now().UTC().Before(g.rootExpires)
}
func (g *Grants) Verify(secret []byte) (GrantSession, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now().UTC()
	g.prune(now)
	if !g.configured || g.rootConsumed || !now.Before(g.rootExpires) {
		return GrantSession{}, ErrInvalidGrant
	}
	got := sha256.Sum256(canonicalSecret(secret))
	if subtle.ConstantTimeCompare(g.rootHash[:], got[:]) != 1 {
		return GrantSession{}, ErrInvalidGrant
	}
	if len(g.records) >= MaxGrantSessions {
		return GrantSession{}, ErrCapacity
	}
	token, err := randomToken(g.random, 32)
	if err != nil {
		return GrantSession{}, err
	}
	id, err := randomToken(g.random, 16)
	if err != nil {
		return GrantSession{}, err
	}
	h := sha256.Sum256([]byte(token))
	rec := &grantRecord{id: id, hash: h, kind: g.kind, expires: now.Add(GrantSessionLifetime)}
	g.records[h] = rec
	g.rootConsumed = true
	return GrantSession{ID: id, Token: token, Kind: g.kind, ExpiresAt: rec.expires}, nil
}
func (g *Grants) Authenticate(token string) (string, error) {
	h := sha256.Sum256([]byte(token))
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prune(g.now().UTC())
	r := g.records[h]
	if r == nil || r.consumed {
		return "", ErrInvalidGrant
	}
	return r.id, nil
}
func (g *Grants) Consume(token string) error {
	h := sha256.Sum256([]byte(token))
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prune(g.now().UTC())
	r := g.records[h]
	if r == nil || r.consumed {
		return ErrInvalidGrant
	}
	r.consumed = true
	delete(g.records, h)
	return nil
}
func (g *Grants) prune(now time.Time) {
	for h, r := range g.records {
		if !now.Before(r.expires) {
			delete(g.records, h)
		}
	}
}

type Ceremony struct {
	ID, CookieToken, Peer    string
	Kind                     CeremonyKind
	GrantID, SessionID, Meta string
	UserHandle               []byte
	Data                     webauthn.SessionData
	ExpiresAt                time.Time
}
type ceremonyRecord struct {
	Ceremony
	hash [32]byte
}
type Ceremonies struct {
	mu         sync.Mutex
	now        func() time.Time
	random     io.Reader
	records    map[[32]byte]*ceremonyRecord
	peerCounts map[string]int
}

func NewCeremonies(now func() time.Time, random io.Reader) *Ceremonies {
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	return &Ceremonies{now: now, random: random, records: map[[32]byte]*ceremonyRecord{}, peerCounts: map[string]int{}}
}
func (c *Ceremonies) Create(kind CeremonyKind, peer, grantID, sessionID, meta string, userHandle []byte, data webauthn.SessionData) (Ceremony, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	c.prune(now)
	peer = normalizePeer(peer)
	if len(c.records) >= MaxCeremonies || c.peerCounts[peer] >= MaxCeremoniesPerPeer {
		return Ceremony{}, ErrCapacity
	}
	token, err := randomToken(c.random, 32)
	if err != nil {
		return Ceremony{}, err
	}
	id, err := randomToken(c.random, 16)
	if err != nil {
		return Ceremony{}, err
	}
	x := Ceremony{ID: id, CookieToken: token, Peer: peer, Kind: kind, GrantID: grantID, SessionID: sessionID, Meta: meta, UserHandle: append([]byte(nil), userHandle...), Data: data, ExpiresAt: now.Add(CeremonyLifetime)}
	h := sha256.Sum256([]byte(token))
	stored := x
	stored.CookieToken = ""
	c.records[h] = &ceremonyRecord{Ceremony: stored, hash: h}
	c.peerCounts[peer]++
	return x, nil
}
func (c *Ceremonies) Consume(token string, kind CeremonyKind, grantID, sessionID string) (Ceremony, error) {
	h := sha256.Sum256([]byte(token))
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	c.prune(now)
	r := c.records[h]
	if r == nil || r.Kind != kind || r.GrantID != grantID || r.SessionID != sessionID {
		return Ceremony{}, ErrInvalidCeremony
	}
	c.delete(h, r)
	return r.Ceremony, nil
}
func (c *Ceremonies) delete(h [32]byte, r *ceremonyRecord) {
	delete(c.records, h)
	c.peerCounts[r.Peer]--
	if c.peerCounts[r.Peer] <= 0 {
		delete(c.peerCounts, r.Peer)
	}
}
func (c *Ceremonies) prune(now time.Time) {
	for h, r := range c.records {
		if !now.Before(r.ExpiresAt) {
			c.delete(h, r)
		}
	}
}
func normalizePeer(peer string) string {
	peer = strings.TrimSpace(peer)
	if host, _, err := net.SplitHostPort(peer); err == nil {
		return host
	}
	return peer
}
