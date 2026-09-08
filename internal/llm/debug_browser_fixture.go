//go:build browserfixture

package llm

import (
	"context"
	"crypto/rand"
	"strings"
	"sync"
)

var debugBrowserGates sync.Map

// DebugBrowserGate holds a real debug-provider response until the browser test
// explicitly releases it. Unique prompts isolate tests sharing a fixture server.
// This control surface exists only in browserfixture builds.
type DebugBrowserGate struct {
	Prompt      string
	started     chan struct{}
	released    chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func NewDebugBrowserGate() *DebugBrowserGate {
	gate := &DebugBrowserGate{
		Prompt:   "browser-response-gate:" + rand.Text(),
		started:  make(chan struct{}),
		released: make(chan struct{}),
	}
	debugBrowserGates.Store(gate.Prompt, gate)
	return gate
}

func LookupDebugBrowserGate(prompt string) (*DebugBrowserGate, bool) {
	value, ok := debugBrowserGates.Load(prompt)
	if !ok {
		return nil, false
	}
	return value.(*DebugBrowserGate), true
}

func (g *DebugBrowserGate) WaitStarted(ctx context.Context) error {
	select {
	case <-g.started:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *DebugBrowserGate) Release() {
	g.releaseOnce.Do(func() {
		debugBrowserGates.Delete(g.Prompt)
		close(g.released)
	})
}

func waitDebugBrowserFixture(ctx context.Context, prompt string) error {
	// User messages may include injected platform/workspace context. Match only
	// a registered, unguessable token, not arbitrary text resembling a command.
	for _, word := range strings.Fields(prompt) {
		gate, ok := LookupDebugBrowserGate(word)
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
