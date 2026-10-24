package peerbroker

import (
	"sync"
	"time"
)

// tokenBucket is a small global request limiter driven by an injectable clock.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newTokenBucket(rate float64, burst int, now func() time.Time) *tokenBucket {
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst), last: now(), now: now}
}

func (b *tokenBucket) allow() bool { return b.allowN(1) }

// allowN takes n tokens if available. A request larger than burst never succeeds.
func (b *tokenBucket) allowN(n float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(b.burst, b.tokens+elapsed*b.rate)
	}
	b.last = now
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// drain empties the bucket so the next allowN fails until it refills.
func (b *tokenBucket) drain() {
	b.mu.Lock()
	b.tokens = 0
	b.last = b.now()
	b.mu.Unlock()
}
