package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

const defaultResponsesWebSocketMaxConnectionsPerKey = 8

var errResponsesWebSocketPoolSaturated = errors.New("Responses WebSocket pool saturated: all connections are active")

type responsesWebSocketPool struct {
	mu        sync.Mutex
	maxPerKey int
	byKey     map[string]map[*responsesWebSocketLease]struct{}
}

type responsesWebSocketLease struct {
	pool     *responsesWebSocketPool
	key      string
	active   bool
	parkedAt time.Time
	conn     *responsesWebSocketConnection
	admitted bool
}

func newResponsesWebSocketPool(maxPerKey int) *responsesWebSocketPool {
	if maxPerKey <= 0 {
		maxPerKey = defaultResponsesWebSocketMaxConnectionsPerKey
	}
	return &responsesWebSocketPool{
		maxPerKey: maxPerKey,
		byKey:     make(map[string]map[*responsesWebSocketLease]struct{}),
	}
}

var sharedResponsesWebSocketPool = newResponsesWebSocketPool(defaultResponsesWebSocketMaxConnectionsPerKey)

func (c *ResponsesClient) websocketAdmissionKey() string {
	if key := strings.TrimSpace(c.WebSocketPoolKey); key != "" {
		return key
	}
	endpoint := strings.TrimSpace(c.WebSocketURL)
	if endpoint == "" {
		endpoint = strings.TrimSpace(c.BaseURL)
	}
	auth := ""
	if c.GetAuthHeader != nil {
		auth = c.GetAuthHeader()
	}
	digest := sha256.Sum256([]byte(endpoint + "\x00" + auth))
	return endpoint + "#" + hex.EncodeToString(digest[:8])
}

func (p *responsesWebSocketPool) acquire(key string) (*responsesWebSocketLease, error) {
	key = strings.TrimSpace(key)
	p.mu.Lock()
	entries := p.byKey[key]
	if entries == nil {
		entries = make(map[*responsesWebSocketLease]struct{})
		p.byKey[key] = entries
	}

	var victim *responsesWebSocketLease
	if len(entries) >= p.maxPerKey {
		for candidate := range entries {
			if candidate.active {
				continue
			}
			if victim == nil || candidate.parkedAt.Before(victim.parkedAt) {
				victim = candidate
			}
		}
		if victim == nil {
			p.mu.Unlock()
			return nil, errResponsesWebSocketPoolSaturated
		}
		delete(entries, victim)
		victim.admitted = false
	}

	lease := &responsesWebSocketLease{pool: p, key: key, active: true, admitted: true}
	entries[lease] = struct{}{}
	victimConn := (*responsesWebSocketConnection)(nil)
	if victim != nil {
		victimConn = victim.conn
	}
	p.mu.Unlock()

	if victimConn != nil {
		_ = victimConn.closeWithError(errors.New("Responses WebSocket parked connection evicted by pool capacity"))
	}
	return lease, nil
}

func (l *responsesWebSocketLease) attach(conn *responsesWebSocketConnection) bool {
	if l == nil || l.pool == nil {
		return true
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if !l.admitted {
		return false
	}
	l.conn = conn
	return true
}

func (l *responsesWebSocketLease) activate() bool {
	if l == nil || l.pool == nil {
		return true
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if !l.admitted {
		return false
	}
	l.active = true
	l.parkedAt = time.Time{}
	return true
}

func (l *responsesWebSocketLease) park() {
	if l == nil || l.pool == nil {
		return
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if !l.admitted {
		return
	}
	l.active = false
	l.parkedAt = time.Now()
}

func (l *responsesWebSocketLease) release() {
	if l == nil || l.pool == nil {
		return
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if !l.admitted {
		return
	}
	entries := l.pool.byKey[l.key]
	delete(entries, l)
	if len(entries) == 0 {
		delete(l.pool.byKey, l.key)
	}
	l.admitted = false
	l.conn = nil
}

func (p *responsesWebSocketPool) count(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byKey[key])
}
