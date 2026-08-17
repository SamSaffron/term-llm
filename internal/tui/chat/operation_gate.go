package chat

import (
	"sync"
	"time"
)

// operationGate keeps runtime-owned resources alive while asynchronous work is
// still using them. Cleanup seals the gate, cancels reservations whose commands
// never started, and waits for operations that actually began.
type operationGate struct {
	mu         sync.Mutex
	operations map[*operationLease]bool // false = reserved, true = running
	sealed     bool
	idle       chan struct{}
}

type operationLease struct {
	gate *operationGate
}

func (g *operationGate) reserve() (*operationLease, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return nil, false
	}
	if len(g.operations) == 0 {
		g.idle = make(chan struct{})
	}
	if g.operations == nil {
		g.operations = make(map[*operationLease]bool)
	}
	lease := &operationLease{gate: g}
	g.operations[lease] = false
	return lease, true
}

// start promotes a reservation to running. It returns false when cleanup sealed
// the gate before Bubble Tea dispatched the command.
func (l *operationLease) start() bool {
	if l == nil || l.gate == nil {
		return false
	}
	g := l.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	running, ok := g.operations[l]
	if !ok || running || g.sealed {
		return false
	}
	g.operations[l] = true
	return true
}

func (l *operationLease) finish() {
	if l == nil || l.gate == nil {
		return
	}
	g := l.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.operations[l]; !ok {
		return
	}
	delete(g.operations, l)
	g.signalIdleLocked()
}

func (g *operationGate) sealAndWait(timeout time.Duration) bool {
	g.mu.Lock()
	g.sealed = true
	for lease, running := range g.operations {
		if !running {
			delete(g.operations, lease)
		}
	}
	g.signalIdleLocked()
	if len(g.operations) == 0 {
		g.mu.Unlock()
		return true
	}
	idle := g.idle
	g.mu.Unlock()

	if timeout <= 0 {
		<-idle
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

func (g *operationGate) wait() {
	g.mu.Lock()
	if len(g.operations) == 0 {
		g.mu.Unlock()
		return
	}
	idle := g.idle
	g.mu.Unlock()
	<-idle
}

func (g *operationGate) signalIdleLocked() {
	if len(g.operations) == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

func (g *operationGate) activeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.operations)
}
