// Package restart owns process-wide restart signal dispatch. A signal never
// implies permission to replay a command: until a mode installs an owner, it is
// deferred and the original invocation is allowed to finish.
package restart

import (
	"log"
	"sync"
)

// Dispatcher keeps the signal disposition installed while mode owners come and
// go. Hooks must return promptly and coalesce their asynchronous drain requests.
// Unbinding joins dispatch of the hook, so a torn-down owner cannot be called.
type Dispatcher struct {
	mu       sync.Mutex
	owner    *binding
	deferred bool
	report   func(string)
}

type binding struct{ request func() }

func New(report func(string)) *Dispatcher { return &Dispatcher{report: report} }

// Default is shared by the executable entry point and per-mode owners.
var Default = New(func(phase string) {
	log.Printf("[reload] SIGUSR2 %s: invocation retained; no replay", phase)
})

func (d *Dispatcher) Request() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.owner != nil {
		d.owner.request()
		return
	}
	if !d.deferred {
		d.deferred = true
		if d.report != nil {
			d.report("deferred")
		}
	}
}

// Bind installs the one process owner. A combined mode must coordinate all its
// components behind this hook; independent subscribers are not permitted.
// Signals received before binding are not replayed into a not-yet-ready mode.
func (d *Dispatcher) Bind(request func()) func() {
	if request == nil {
		panic("restart: nil owner")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.owner != nil {
		panic("restart: process already has an owner")
	}
	b := &binding{request: request}
	d.owner = b
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.owner == b {
			d.owner = nil
		}
	}
}

// Finish makes a deferred short-lived command's completion observable without
// starting a second invocation. It must run before os.Exit.
func (d *Dispatcher) Finish() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deferred && d.report != nil {
		d.report("finished")
	}
}
