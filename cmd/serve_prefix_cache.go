package cmd

import (
	"hash/fnv"
	"regexp"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// prefixHashCache maps FNV-64a hashes of message prefixes to session IDs.
// This enables reusing an existing session when a new request arrives with
// the same conversation prefix, which preserves the Responses API
// previous_response_id chain and enables server-side prompt caching.
type prefixHashCache struct {
	mu      sync.Mutex
	entries map[uint64]prefixHashEntry
}

type prefixHashEntry struct {
	sessionID string
	expires   time.Time
}

func newPrefixHashCache() *prefixHashCache {
	return &prefixHashCache{
		entries: make(map[uint64]prefixHashEntry),
	}
}

// computeMessageHashes returns a running FNV-64a hash after each message.
// Because FNV is a streaming hash, the hash at position i naturally
// incorporates messages 0..i, so the slice has the incremental-prefix
// property: hashes[:k] of [m1..mN] equals hashes[:k] of [m1..mN, mN+1..].
func computeMessageHashes(messages []llm.Message, stripPatterns []*regexp.Regexp) []uint64 {
	h := fnv.New64a()
	out := make([]uint64, len(messages))
	delim := []byte{0xFF}

	for i, msg := range messages {
		// Role
		h.Write([]byte(string(msg.Role)))
		h.Write(delim)

		for _, p := range msg.Parts {
			// Part type
			h.Write([]byte(string(p.Type)))
			h.Write(delim)

			// First 256 bytes of text (after stripping dynamic patterns)
			if p.Text != "" {
				text := p.Text
				for _, re := range stripPatterns {
					text = re.ReplaceAllString(text, "")
				}
				if len(text) > 256 {
					text = text[:256]
				}
				h.Write([]byte(text))
				h.Write(delim)
			}

			// ToolCall fields
			if p.ToolCall != nil {
				h.Write([]byte(p.ToolCall.ID))
				h.Write(delim)
				h.Write([]byte(p.ToolCall.Name))
				h.Write(delim)
			}

			// ToolResult fields
			if p.ToolResult != nil {
				h.Write([]byte(p.ToolResult.ID))
				h.Write(delim)
			}
		}

		out[i] = h.Sum64()
	}
	return out
}

// Match scans hashes from the end backward (up to 20 entries) looking for a
// cached session. Returns the session ID, the matched prefix length, and
// whether a match was found. Expired entries are lazily deleted.
func (c *prefixHashCache) Match(hashes []uint64) (sessionID string, matchedCount int, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	start := len(hashes) - 1
	stop := start - 20
	if stop < 0 {
		stop = 0
	}

	for i := start; i >= stop; i-- {
		entry, ok := c.entries[hashes[i]]
		if !ok {
			continue
		}
		if now.After(entry.expires) {
			// Lazy-delete expired entry
			delete(c.entries, hashes[i])
			continue
		}
		return entry.sessionID, i + 1, true
	}
	return "", 0, false
}

// Store records a hash→session mapping with the given TTL.
func (c *prefixHashCache) Store(hash uint64, sessionID string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[hash] = prefixHashEntry{
		sessionID: sessionID,
		expires:   time.Now().Add(ttl),
	}
}

// EvictSession removes all cache entries that point to the given session.
// Called when a session is evicted from the session manager.
func (c *prefixHashCache) EvictSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for h, entry := range c.entries {
		if entry.sessionID == sessionID {
			delete(c.entries, h)
		}
	}
}

// CleanExpired removes all entries whose TTL has passed.
func (c *prefixHashCache) CleanExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for h, entry := range c.entries {
		if now.After(entry.expires) {
			delete(c.entries, h)
		}
	}
}

// Len returns the number of entries in the cache (for diagnostics).
func (c *prefixHashCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// StoredHashes returns all stored hash keys (for diagnostics).
func (c *prefixHashCache) StoredHashes() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]uint64, 0, len(c.entries))
	for h := range c.entries {
		keys = append(keys, h)
	}
	return keys
}
