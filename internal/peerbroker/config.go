package peerbroker

import (
	"fmt"
	"time"
)

// Session TTL bounds. Sessions are rendezvous points only; long-lived
// connectivity belongs to the peers themselves.
const (
	MinSessionTTL     = time.Minute
	DefaultSessionTTL = 10 * time.Minute
	MaxSessionTTL     = 30 * time.Minute
)

// TURN credential TTL bounds accepted by the broker and the Cloudflare adapter.
const (
	MinTURNTTL     = 5 * time.Minute
	DefaultTURNTTL = 10 * time.Minute
	MaxTURNTTL     = 30 * time.Minute
)

const (
	defaultMaxSessions           = 1000
	defaultMaxPendingMessages    = 32
	defaultMaxMessagesPerSession = 256
	defaultMaxMessageBytes       = 64 << 10
	defaultMaxWaiters            = 512
	defaultMaxWait               = 25 * time.Second
	defaultRatePerSecond         = 50
	defaultRateBurst             = 100
	defaultMaxTURNPerSession     = 4
	defaultMaxConnections        = 512
	defaultMaxConnectsPerSession = 16
	defaultWSFramesPerSecond     = 10
	defaultWSFrameBurst          = 32
	defaultWSBytesPerSecond      = 64 << 10
	defaultWSMaxViolations       = 8

	hardMaxMessageBytes = 1 << 20
	hardMaxWait         = 60 * time.Second
	hardMaxPending      = 1024
)

// Config bounds broker resource use. Zero values select defaults; negative
// values are rejected.
type Config struct {
	// SessionTTL is the default and maximum lifetime of a session
	// (default 10m, clamped to [1m, 30m]). Create may request a shorter TTL.
	SessionTTL time.Duration
	// MaxSessions bounds concurrently live sessions (default 1000).
	MaxSessions int
	// MaxPendingMessages bounds unacknowledged messages per mailbox (default 32).
	MaxPendingMessages int
	// MaxMessagesPerSession bounds total sends across a session (default 256).
	MaxMessagesPerSession int
	// MaxMessageBytes bounds one message body (default 64 KiB, max 1 MiB).
	MaxMessageBytes int
	// MaxWaiters bounds concurrent long-poll receives globally (default 512).
	MaxWaiters int
	// MaxWait bounds a single long-poll (default 25s, max 60s).
	MaxWait time.Duration
	// MaxConnections bounds concurrent WebSocket registrations globally (default 512).
	MaxConnections int
	// MaxConnectsPerSession bounds WebSocket registrations per session (default 16).
	MaxConnectsPerSession int
	// WSFramesPerSecond/WSFrameBurst bound inbound WebSocket data messages
	// (charged when the first frame header arrives, before the payload is
	// read) and ping/pong control frames per connection (defaults 10/s, burst
	// 32). This is a message quota, not a frame-header quota; continuation
	// frames are bounded by the raw wire budget below. Exceeding it closes
	// with WSCloseRateLimited.
	WSFramesPerSecond float64
	WSFrameBurst      int
	// WSBytesPerSecond/WSByteBurst bound inbound message payload bytes per
	// connection (defaults 64 KiB/s; burst at least one maximal message).
	// Raw bytes read from the upgraded socket (all frame headers included)
	// are separately budgeted at these values plus 14 bytes per allowed frame;
	// exhausting either closes with WSCloseRateLimited.
	WSBytesPerSecond float64
	WSByteBurst      int
	// WSMaxViolations is the number of rejected frames (malformed, binary,
	// oversized body, mailbox full, global rate) tolerated per connection
	// before it is closed with WSCloseViolation (default 8).
	WSMaxViolations int
	// RatePerSecond and RateBurst configure the global request token bucket
	// (defaults 50/s, burst 100).
	RatePerSecond float64
	RateBurst     int
	// CreateToken is the private registration capability required (as a
	// Bearer) to create sessions. It is mandatory: NewHandler fails closed
	// without one unless AllowUnauthenticatedCreate is set. It must be at least
	// MinCreateTokenLen printable non-space ASCII characters, and it is
	// independent of any per-session capability or pairing secret.
	CreateToken string
	// AllowUnauthenticatedCreate permits anonymous session creation (still
	// subject to every other limit). Intended for tests/local development only.
	AllowUnauthenticatedCreate bool

	// TURNMinter optionally enables POST /v1/sessions/{id}/turn.
	TURNMinter TURNMinter
	// TURNTTL is the credential lifetime requested from TURNMinter (default 10m, 5m-30m).
	TURNTTL time.Duration
	// MaxTURNPerSession bounds mint attempts per session (default 4).
	MaxTURNPerSession int

	// Now overrides the clock (tests).
	Now func() time.Time
}

func (c Config) withDefaults() (Config, error) {
	if c.SessionTTL < 0 || c.MaxSessions < 0 || c.MaxPendingMessages < 0 || c.MaxMessagesPerSession < 0 ||
		c.MaxMessageBytes < 0 || c.MaxWaiters < 0 || c.MaxWait < 0 || c.RatePerSecond < 0 || c.RateBurst < 0 ||
		c.TURNTTL < 0 || c.MaxTURNPerSession < 0 || c.MaxConnections < 0 || c.MaxConnectsPerSession < 0 ||
		c.WSFramesPerSecond < 0 || c.WSFrameBurst < 0 || c.WSBytesPerSecond < 0 || c.WSByteBurst < 0 || c.WSMaxViolations < 0 {
		return c, fmt.Errorf("peerbroker: negative config value")
	}
	if err := validateCreateToken(c.CreateToken, c.AllowUnauthenticatedCreate); err != nil {
		return c, err
	}
	c.SessionTTL = clampDuration(defaultDuration(c.SessionTTL, DefaultSessionTTL), MinSessionTTL, MaxSessionTTL)
	c.MaxSessions = defaultInt(c.MaxSessions, defaultMaxSessions)
	c.MaxPendingMessages = min(defaultInt(c.MaxPendingMessages, defaultMaxPendingMessages), hardMaxPending)
	c.MaxMessagesPerSession = defaultInt(c.MaxMessagesPerSession, defaultMaxMessagesPerSession)
	c.MaxMessageBytes = min(defaultInt(c.MaxMessageBytes, defaultMaxMessageBytes), hardMaxMessageBytes)
	c.MaxWaiters = defaultInt(c.MaxWaiters, defaultMaxWaiters)
	c.MaxConnections = defaultInt(c.MaxConnections, defaultMaxConnections)
	c.MaxConnectsPerSession = defaultInt(c.MaxConnectsPerSession, defaultMaxConnectsPerSession)
	c.MaxWait = min(defaultDuration(c.MaxWait, defaultMaxWait), hardMaxWait)
	if c.RatePerSecond == 0 {
		c.RatePerSecond = defaultRatePerSecond
	}
	c.RateBurst = defaultInt(c.RateBurst, defaultRateBurst)
	if c.WSFramesPerSecond == 0 {
		c.WSFramesPerSecond = defaultWSFramesPerSecond
	}
	c.WSFrameBurst = defaultInt(c.WSFrameBurst, defaultWSFrameBurst)
	if c.WSBytesPerSecond == 0 {
		c.WSBytesPerSecond = defaultWSBytesPerSecond
	}
	c.WSByteBurst = max(defaultInt(c.WSByteBurst, 2*(c.MaxMessageBytes+wsFrameOverhead)), c.MaxMessageBytes+wsFrameOverhead)
	c.WSMaxViolations = defaultInt(c.WSMaxViolations, defaultWSMaxViolations)
	c.TURNTTL = defaultDuration(c.TURNTTL, DefaultTURNTTL)
	if c.TURNTTL < MinTURNTTL || c.TURNTTL > MaxTURNTTL {
		return c, fmt.Errorf("peerbroker: TURN TTL %s outside [%s, %s]", c.TURNTTL, MinTURNTTL, MaxTURNTTL)
	}
	c.MaxTURNPerSession = defaultInt(c.MaxTURNPerSession, defaultMaxTURNPerSession)
	if c.Now == nil {
		c.Now = time.Now
	}
	return c, nil
}

// MinCreateTokenLen is the minimum length of Config.CreateToken
// (e.g. 32 random bytes base64url-encoded is 43 characters).
const MinCreateTokenLen = 32

func validateCreateToken(tok string, allowAnon bool) error {
	if tok == "" {
		if allowAnon {
			return nil
		}
		return fmt.Errorf("peerbroker: CreateToken is required (set AllowUnauthenticatedCreate only for tests)")
	}
	if len(tok) < MinCreateTokenLen || len(tok) > maxBearerLen {
		return fmt.Errorf("peerbroker: CreateToken must be %d-%d characters", MinCreateTokenLen, maxBearerLen)
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] <= ' ' || tok[i] > '~' {
			return fmt.Errorf("peerbroker: CreateToken must be printable non-space ASCII")
		}
	}
	return nil
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func defaultDuration(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}
	return v
}

func clampDuration(v, lo, hi time.Duration) time.Duration {
	return max(lo, min(v, hi))
}
