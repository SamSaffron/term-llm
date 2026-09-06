// Package process provides advisory process discovery, never restart authority.
package process

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Record struct {
	PID        int    `json:"pid"`
	Start      string `json:"os_start"`
	Instance   string `json:"instance"`
	Build      string `json:"build"`
	BuildID    string `json:"build_id"`
	Executable string `json:"executable"`
	Mode       string `json:"mode"`
	Phase      string `json:"phase"`
	Detail     string `json:"detail,omitempty"`
}

// Publisher has no filesystem work on the command startup path. One worker owns
// publication AND final removal: a timed-out Stop cannot race a late writer into
// resurrecting a record after cleanup. An abruptly exited worker leaves only an
// advisory stale record, which List validates against the OS.
type Publisher struct {
	mu         sync.Mutex
	record     Record
	wake       chan struct{}
	cancel     context.CancelFunc
	done       chan struct{}
	publishing bool // guarded by mu; false means Stop need not join filesystem work
}

var currentMu sync.RWMutex
var current *Publisher

func Start(build string) *Publisher {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Publisher{record: Record{PID: os.Getpid(), Instance: uuid.NewString(), Build: build, Mode: "command", Phase: "starting"}, wake: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{})}
	currentMu.Lock()
	current = p
	currentMu.Unlock()
	go p.run(ctx)
	return p
}

// State only changes memory and coalesces a notification; it never waits for I/O.
func State(mode, phase, detail string) {
	currentMu.RLock()
	p := current
	currentMu.RUnlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	if mode != "" {
		p.record.Mode = mode
	}
	p.record.Phase, p.record.Detail = phase, detail
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Publisher) Stop() {
	p.mu.Lock()
	p.cancel()
	publishing := p.publishing
	p.mu.Unlock()
	if publishing {
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
		}
	}
	currentMu.Lock()
	if current == p {
		current = nil
	}
	currentMu.Unlock()
}

var ErrUnsupported = errors.New("safe process discovery/restart requires Linux procfs and pidfd support")

// Instance returns the in-memory executable incarnation without registry I/O.
func Instance() string {
	currentMu.RLock()
	p := current
	currentMu.RUnlock()
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.record.Instance
}
