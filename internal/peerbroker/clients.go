package peerbroker

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Public open-admission defaults. They bound one client network and the whole
// process without a shared admission secret; host identity proofs, one-use
// invitations and per-lease signalling budgets still apply unchanged.
const (
	DefaultPublicConnectionLifetime = 12 * time.Hour
	defaultMaxTrackedClients        = 4096
	clientSweepInterval             = 10 * time.Second
	clientIdleRetention             = 2 * time.Minute
)

// ClientPolicy bounds the resources one client network may consume. It is
// intended for open-admission deployments where host registration has no
// private credential. IPv4 clients are keyed by address and IPv6 clients by
// /64. A zero numeric field disables that individual limit.
type ClientPolicy struct {
	RequestRate    float64
	RequestBurst   int
	MaxChallenges  int
	MaxHosts       int
	MaxConnections int
	// MaxClients bounds the tracked client table; zero selects 4096. New
	// clients are refused while every tracked client still holds resources.
	MaxClients int
	// TrustedProxies lists reverse-proxy peers whose X-Real-IP header names
	// the client. Requests from other peers use their socket address.
	TrustedProxies []netip.Prefix
}

// PublicClientPolicy returns the per-client limits used by `peer-broker
// --public`. They admit ordinary hosts behind shared NAT while preventing one
// network from exhausting the global host, challenge or socket tables.
func PublicClientPolicy() ClientPolicy {
	return ClientPolicy{RequestRate: 2, RequestBurst: 30, MaxChallenges: 4, MaxHosts: 16, MaxConnections: 64}
}

func (p ClientPolicy) withDefaults() (ClientPolicy, error) {
	if p.RequestRate < 0 || p.RequestBurst < 0 || p.MaxChallenges < 0 || p.MaxHosts < 0 || p.MaxConnections < 0 || p.MaxClients < 0 {
		return p, errors.New("peerbroker: negative client policy value")
	}
	if p.RequestRate > 0 && p.RequestBurst == 0 {
		p.RequestBurst = max(1, int(p.RequestRate))
	}
	p.MaxClients = defaultInt(p.MaxClients, defaultMaxTrackedClients)
	p.TrustedProxies = append([]netip.Prefix(nil), p.TrustedProxies...)
	for i, prefix := range p.TrustedProxies {
		if !prefix.IsValid() {
			return p, fmt.Errorf("peerbroker: trusted proxy %d is invalid", i)
		}
		p.TrustedProxies[i] = prefix.Masked()
	}
	return p, nil
}

// ParseTrustedProxy accepts an IP address or CIDR prefix.
func ParseTrustedProxy(raw string) (netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if prefix, err := netip.ParsePrefix(raw); err == nil {
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("peerbroker: trusted proxy %q must be an IP address or CIDR prefix", raw)
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

type clientUsage struct {
	limiter     *tokenBucket
	challenges  int
	hosts       int
	connections int
	lastSeen    time.Time
}

func (u *clientUsage) idle() bool { return u.challenges == 0 && u.hosts == 0 && u.connections == 0 }

type clientRegistry struct {
	policy    ClientPolicy
	clients   map[string]*clientUsage
	lastSweep time.Time
}

func newClientRegistry(policy *ClientPolicy) *clientRegistry {
	if policy == nil {
		return nil
	}
	return &clientRegistry{policy: *policy, clients: map[string]*clientUsage{}}
}

// clientKey names the network that sent r. It returns "" when per-client
// limits are disabled.
func (h *Handler) clientKey(r *http.Request) string {
	if h.clients == nil {
		return ""
	}
	addr, _ := netip.ParseAddrPort(r.RemoteAddr)
	ip := addr.Addr().Unmap()
	if ip.IsValid() && h.clients.trusted(ip) {
		if forwarded, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
			ip = forwarded.Unmap()
		}
	}
	if !ip.IsValid() {
		return "unknown"
	}
	if ip.Is4() {
		return ip.String()
	}
	prefix, _ := ip.Prefix(64)
	return prefix.String()
}

func (c *clientRegistry) trusted(ip netip.Addr) bool {
	for _, prefix := range c.policy.TrustedProxies {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// clientUsageLocked returns the tracked usage for key, creating it if capacity
// allows. It returns (nil, true) when limits are disabled and (nil, false)
// when the client table is full.
func (h *Handler) clientUsageLocked(key string) (*clientUsage, bool) {
	if h.clients == nil || key == "" {
		return nil, true
	}
	now := h.cfg.Now()
	if usage := h.clients.clients[key]; usage != nil {
		usage.lastSeen = now
		return usage, true
	}
	if len(h.clients.clients) >= h.clients.policy.MaxClients {
		h.sweepClientsLocked(now, 0)
		if len(h.clients.clients) >= h.clients.policy.MaxClients {
			return nil, false
		}
	}
	usage := &clientUsage{lastSeen: now}
	if h.clients.policy.RequestRate > 0 {
		usage.limiter = newTokenBucket(h.clients.policy.RequestRate, h.clients.policy.RequestBurst, h.cfg.Now)
	}
	h.clients.clients[key] = usage
	return usage, true
}

// allowClientRequest applies the per-client request rate before the shared
// process-wide bucket, so one network cannot drain capacity for others.
func (h *Handler) allowClientRequest(key string) bool {
	h.mu.Lock()
	usage, ok := h.clientUsageLocked(key)
	h.mu.Unlock()
	if !ok {
		return false
	}
	return usage == nil || usage.limiter == nil || usage.limiter.allow()
}

func (h *Handler) sweepClientsLocked(now time.Time, retention time.Duration) {
	if h.clients == nil {
		return
	}
	h.clients.lastSweep = now
	for key, usage := range h.clients.clients {
		if usage.idle() && now.Sub(usage.lastSeen) >= retention {
			delete(h.clients.clients, key)
		}
	}
}

func (h *Handler) maybeSweepClientsLocked(now time.Time) {
	if h.clients != nil && now.Sub(h.clients.lastSweep) >= clientSweepInterval {
		h.sweepClientsLocked(now, clientIdleRetention)
	}
}

func (h *Handler) clientLocked(key string) *clientUsage {
	if h.clients == nil || key == "" {
		return nil
	}
	return h.clients.clients[key]
}

func (h *Handler) acquireChallengeLocked(key string) bool {
	usage, ok := h.clientUsageLocked(key)
	if !ok {
		return false
	}
	if usage == nil {
		return true
	}
	if limit := h.clients.policy.MaxChallenges; limit > 0 && usage.challenges >= limit {
		return false
	}
	usage.challenges++
	return true
}

func (h *Handler) releaseChallengeLocked(key string) {
	if usage := h.clientLocked(key); usage != nil && usage.challenges > 0 {
		usage.challenges--
	}
}

// hostCapacityLocked reports whether key may hold one more host lease. A
// re-registration that replaces a lease already held by key is not growth.
func (h *Handler) hostCapacityLocked(key string, replaced *hostLease) bool {
	usage, ok := h.clientUsageLocked(key)
	if !ok {
		return false
	}
	if usage == nil || replaced != nil && replaced.client == key {
		return true
	}
	limit := h.clients.policy.MaxHosts
	return limit == 0 || usage.hosts < limit
}

func (h *Handler) acquireHostLocked(lease *hostLease) {
	if usage := h.clientLocked(lease.client); usage != nil {
		usage.hosts++
	}
}

func (h *Handler) releaseHostLocked(lease *hostLease) {
	if usage := h.clientLocked(lease.client); usage != nil && usage.hosts > 0 {
		usage.hosts--
	}
}

func (h *Handler) acquireConnectionLocked(key string) bool {
	if h.connections >= h.cfg.MaxConnections {
		return false
	}
	usage, ok := h.clientUsageLocked(key)
	if !ok {
		return false
	}
	if usage != nil {
		if limit := h.clients.policy.MaxConnections; limit > 0 && usage.connections >= limit {
			return false
		}
		usage.connections++
	}
	h.connections++
	return true
}

func (h *Handler) releaseConnectionLocked(key string) {
	h.connections--
	if usage := h.clientLocked(key); usage != nil && usage.connections > 0 {
		usage.connections--
	}
}
