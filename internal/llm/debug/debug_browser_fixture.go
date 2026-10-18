//go:build browserfixture

package debug

import (
	"context"
	"crypto/rand"
	"strings"
	"sync"
)

var debugBrowserGates sync.Map

// BrowserGate holds a real debug-provider response until the browser test
// explicitly releases it. Unique prompts isolate tests sharing a fixture server.
// This control surface exists only in browserfixture builds.
type BrowserGate struct {
	Prompt      string
	started     chan struct{}
	released    chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func NewBrowserGate() *BrowserGate {
	gate := &BrowserGate{
		Prompt:   "browser-response-gate:" + rand.Text(),
		started:  make(chan struct{}),
		released: make(chan struct{}),
	}
	debugBrowserGates.Store(gate.Prompt, gate)
	return gate
}

func LookupBrowserGate(prompt string) (*BrowserGate, bool) {
	value, ok := debugBrowserGates.Load(prompt)
	if !ok {
		return nil, false
	}
	return value.(*BrowserGate), true
}

func (g *BrowserGate) WaitStarted(ctx context.Context) error {
	select {
	case <-g.started:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *BrowserGate) Release() {
	g.releaseOnce.Do(func() {
		debugBrowserGates.Delete(g.Prompt)
		close(g.released)
	})
}

func waitDebugBrowserFixture(ctx context.Context, prompt string) error {
	// User messages may include injected platform/workspace context. Match only
	// a registered, unguessable token, not arbitrary text resembling a command.
	for _, word := range strings.Fields(prompt) {
		gate, ok := LookupBrowserGate(word)
		if !ok {
			continue
		}
		gate.startOnce.Do(func() { close(gate.started) })
		select {
		case <-gate.released:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
