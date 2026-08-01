package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

type providerSessionKey struct {
	clientID  string
	provider  string
	sessionID string
}

type providerSessionEntry struct {
	token       chan struct{}
	provider    llm.Provider
	fingerprint [sha256.Size]byte
	expiresAt   time.Time
}

type providerSessionLease struct {
	cache    *providerSessionCache
	key      providerSessionKey
	entry    *providerSessionEntry
	provider llm.Provider
	reused   bool
	once     sync.Once
}

type providerSessionCache struct {
	idleTimeout time.Duration

	mu            sync.Mutex
	entries       map[providerSessionKey]*providerSessionEntry
	closed        bool
	reaperStarted bool
	stop          chan struct{}
	done          chan struct{}
	close         sync.Once
}

func newProviderSessionCache(idleTimeout time.Duration) *providerSessionCache {
	cache := &providerSessionCache{
		idleTimeout: idleTimeout,
		entries:     make(map[providerSessionKey]*providerSessionEntry),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	return cache
}

func (c *providerSessionCache) acquire(ctx context.Context, key providerSessionKey, fingerprint [sha256.Size]byte) (*providerSessionLease, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, fmt.Errorf("gateway provider session cache is closed")
		}
		entry := c.entries[key]
		if entry == nil {
			entry = &providerSessionEntry{token: make(chan struct{}, 1)}
			entry.token <- struct{}{}
			c.entries[key] = entry
		}
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-entry.token:
		}

		c.mu.Lock()
		current := !c.closed && c.entries[key] == entry
		c.mu.Unlock()
		if !current {
			entry.token <- struct{}{}
			continue
		}

		reused := entry.provider != nil && entry.fingerprint == fingerprint && time.Now().Before(entry.expiresAt)
		if entry.provider != nil && !reused {
			cleanupProviderSession(entry.provider)
			entry.provider = nil
			entry.expiresAt = time.Time{}
		}
		return &providerSessionLease{cache: c, key: key, entry: entry, provider: entry.provider, reused: reused}, nil
	}
}

func (l *providerSessionLease) setProvider(provider llm.Provider, fingerprint [sha256.Size]byte) {
	l.provider = provider
	l.entry.provider = provider
	l.entry.fingerprint = fingerprint
}

func (l *providerSessionLease) release(success bool) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.cache.mu.Lock()
		current := !l.cache.closed && l.cache.entries[l.key] == l.entry
		if !success || !current {
			if l.cache.entries[l.key] == l.entry {
				delete(l.cache.entries, l.key)
			}
		}
		l.cache.mu.Unlock()

		if success && current {
			l.entry.expiresAt = time.Now().Add(l.cache.idleTimeout)
			l.cache.startReaper()
		} else if l.entry.provider != nil {
			cleanupProviderSession(l.entry.provider)
			l.entry.provider = nil
			l.entry.expiresAt = time.Time{}
		}
		l.entry.token <- struct{}{}
	})
}

func (c *providerSessionCache) evict(ctx context.Context, key providerSessionKey) error {
	c.mu.Lock()
	entry := c.entries[key]
	c.mu.Unlock()
	if entry == nil {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-entry.token:
	}
	c.mu.Lock()
	if c.entries[key] == entry {
		delete(c.entries, key)
	}
	c.mu.Unlock()
	if entry.provider != nil {
		cleanupProviderSession(entry.provider)
		entry.provider = nil
		entry.expiresAt = time.Time{}
	}
	entry.token <- struct{}{}
	return nil
}

func (c *providerSessionCache) startReaper() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.reaperStarted {
		return
	}
	c.reaperStarted = true
	go c.reap()
}

func (c *providerSessionCache) reap() {
	defer close(c.done)
	interval := c.idleTimeout / 2
	if interval > time.Second {
		interval = time.Second
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.reapExpired(time.Now())
		}
	}
}

func (c *providerSessionCache) reapExpired(now time.Time) {
	c.mu.Lock()
	entries := make(map[providerSessionKey]*providerSessionEntry, len(c.entries))
	for key, entry := range c.entries {
		entries[key] = entry
	}
	c.mu.Unlock()

	for key, entry := range entries {
		select {
		case <-entry.token:
			c.mu.Lock()
			expired := c.entries[key] == entry && entry.provider != nil && !now.Before(entry.expiresAt)
			if expired {
				delete(c.entries, key)
			}
			c.mu.Unlock()
			if expired {
				cleanupProviderSession(entry.provider)
				entry.provider = nil
				entry.expiresAt = time.Time{}
			}
			entry.token <- struct{}{}
		default:
		}
	}
}

func (c *providerSessionCache) Close() {
	if c == nil {
		return
	}
	c.close.Do(func() {
		c.mu.Lock()
		c.closed = true
		reaperStarted := c.reaperStarted
		entries := make([]*providerSessionEntry, 0, len(c.entries))
		for key, entry := range c.entries {
			entries = append(entries, entry)
			delete(c.entries, key)
		}
		c.mu.Unlock()

		if reaperStarted {
			close(c.stop)
			<-c.done
		}

		for _, entry := range entries {
			<-entry.token
			if entry.provider != nil {
				cleanupProviderSession(entry.provider)
				entry.provider = nil
			}
			entry.token <- struct{}{}
		}
	})
}

func cleanupProviderSession(provider llm.Provider) {
	if resetter, ok := provider.(interface{ ResetConversation() }); ok {
		resetter.ResetConversation()
	}
	if cleaner, ok := provider.(llm.ProviderCleaner); ok {
		cleaner.CleanupMCP()
	}
}

func providerSessionFingerprint(providerKey string, providerType config.ProviderType, providerConfig config.ProviderConfig, generation uint64) [sha256.Size]byte {
	data, _ := json.Marshal(struct {
		ProviderKey  string
		ProviderType config.ProviderType
		Config       config.ProviderConfig
		Generation   uint64
	}{
		ProviderKey: providerKey, ProviderType: providerType, Config: providerConfig, Generation: generation,
	})
	return sha256.Sum256(data)
}
