// Package hub holds the Hub server implementation behind explicit options.
package hub

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const maxAuthPeers = 4096

type ClientPeerResolver struct {
	trusted []netip.Prefix
}

func NewClientPeerResolver(values []string) (*ClientPeerResolver, error) {
	resolver := &ClientPeerResolver{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			addr, addrErr := netip.ParseAddr(value)
			if addrErr != nil {
				return nil, fmt.Errorf("invalid trusted proxy %q: use an IP address or CIDR prefix", value)
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		resolver.trusted = append(resolver.trusted, prefix.Masked())
	}
	return resolver, nil
}

func (r *ClientPeerResolver) Peer(request *http.Request) string {
	direct, ok := parsePeerAddr(request.RemoteAddr)
	if !ok {
		return strings.TrimSpace(request.RemoteAddr)
	}
	if r == nil || !r.isTrusted(direct) {
		return direct.String()
	}
	forwarded := strings.Join(request.Header.Values("X-Forwarded-For"), ",")
	parts := strings.Split(forwarded, ",")
	chain := make([]netip.Addr, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return direct.String()
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			return direct.String()
		}
		chain = append(chain, addr.Unmap())
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if !r.isTrusted(chain[i]) {
			return chain[i].String()
		}
	}
	if len(chain) != 0 {
		return chain[0].String()
	}
	return direct.String()
}

func (r *ClientPeerResolver) isTrusted(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range r.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func parsePeerAddr(remote string) (netip.Addr, bool) {
	remote = strings.TrimSpace(remote)
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	addr, err := netip.ParseAddr(remote)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

type tokenBucket struct {
	tokens, burst, perSecond float64
	last                     time.Time
}

func newTokenBucket(perMinute, burst float64, now time.Time) tokenBucket {
	return tokenBucket{tokens: burst, burst: burst, perSecond: perMinute / 60, last: now}
}
func (b *tokenBucket) allow(now time.Time) bool {
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed*b.perSecond)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type AuthLimiter struct {
	mu     sync.Mutex
	now    func() time.Time
	global tokenBucket
	peers  map[string]*peerLimiter
}
type peerLimiter struct {
	bucket tokenBucket
	last   time.Time
}

func NewAuthLimiter(now func() time.Time) *AuthLimiter {
	if now == nil {
		now = time.Now
	}
	at := now()
	return &AuthLimiter{now: now, global: newTokenBucket(100, 20, at), peers: map[string]*peerLimiter{}}
}
func (l *AuthLimiter) Allow(remote string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	peer := strings.TrimSpace(remote)
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	p := l.peers[peer]
	if p == nil {
		if len(l.peers) >= maxAuthPeers {
			for key, v := range l.peers {
				if now.Sub(v.last) > 10*time.Minute {
					delete(l.peers, key)
				}
			}
		}
		if len(l.peers) >= maxAuthPeers {
			return false
		}
		value := newTokenBucket(10, 5, now)
		p = &peerLimiter{bucket: value}
		l.peers[peer] = p
	}
	if !p.bucket.allow(now) {
		p.last = now
		return false
	}
	if !l.global.allow(now) {
		p.tokensRefund()
		return false
	}
	p.last = now
	return true
}
func (p *peerLimiter) tokensRefund() { p.bucket.tokens = min(p.bucket.burst, p.bucket.tokens+1) }
