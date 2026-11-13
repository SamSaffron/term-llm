package core

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// ClientToolLeaseRegistry retains bounded leases for foreground transport owners.
// Its zero value is ready to use. Hosts must serialize actual owner validation
// and publication with their lifecycle lock; this registry owns only leases.
type ClientToolLeaseRegistry struct {
	mu     sync.Mutex
	leases map[*ClientToolLease]int
}

func (r *ClientToolLeaseRegistry) Admit(binding ClientToolBinding, offered, current []ToolSpec, owner, limit int) *ClientToolLease {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.leases) >= limit {
		return nil
	}
	a, errA := json.Marshal(offered)
	b, errB := json.Marshal(current)
	if errA != nil || errB != nil || string(a) != string(b) {
		return nil
	}
	lease, err := NewClientToolLease(binding, offered)
	if err != nil {
		return nil
	}
	if r.leases == nil {
		r.leases = make(map[*ClientToolLease]int)
	}
	r.leases[lease] = owner
	return lease
}

// Revoke ends all leases for owner, or every owner if owner is negative.
func (r *ClientToolLeaseRegistry) Revoke(reason string, owner int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for lease, id := range r.leases {
		if owner < 0 || owner == id {
			lease.Revoke(reason)
			delete(r.leases, lease)
		}
	}
}

func (r *ClientToolLeaseRegistry) End(lease *ClientToolLease) {
	if lease == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lease.Revoke("originating response ended")
	delete(r.leases, lease)
}

// ClientToolWait keeps an inline device request observable while the host waits.
// Resend must repeat the SAME idempotent request, never dispatch a new action.
type ClientToolWait struct {
	Timeout        time.Duration
	ResendInterval time.Duration
	TickInterval   time.Duration
	Resend         func()
	Tick           func()
}

func (w ClientToolWait) Await(ctx context.Context, done <-chan struct{}) error {
	resend, tick := time.NewTicker(w.ResendInterval), time.NewTicker(w.TickInterval)
	defer resend.Stop()
	defer tick.Stop()
	timeout := time.NewTimer(w.Timeout)
	defer timeout.Stop()
	for {
		select {
		case <-done:
			return nil
		case <-resend.C:
			w.Resend()
		case <-tick.C:
			if w.Tick != nil {
				w.Tick()
			}
		case <-timeout.C:
			return errors.New("the device did not run its tools in time")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ClientToolResultDigest keeps result tombstones fixed-size. Length-prefixing
// every field distinguishes all combinations of errors and ordered outputs.
func ClientToolResultDigest(message string, outputs []string) [sha256.Size]byte {
	hash := sha256.New()
	write := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		hash.Write(size[:])
		hash.Write([]byte(value))
	}
	write(message)
	for _, output := range outputs {
		write(output)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}
