package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// Immutable selections contain no runtime, provider, manager, or store references.
type sessionInputSelection struct{ BasePrompt, Prompt, Tools string }
type sessionInputBinding struct{ Agent, Dir string }
type sessionInputEntry struct {
	binding   sessionInputBinding
	done      chan struct{}
	selection *sessionInputSelection
}
type sessionInputCoordinator struct {
	mu      sync.Mutex
	entries map[string]*sessionInputEntry
	pairs   map[sessionInputSelection]*sessionInputSelection
	strings map[string]string
}

func newSessionInputCoordinator() *sessionInputCoordinator {
	return &sessionInputCoordinator{entries: make(map[string]*sessionInputEntry), pairs: make(map[sessionInputSelection]*sessionInputSelection), strings: make(map[string]string)}
}

var processSessionInputs = newSessionInputCoordinator()

type sessionInputTicket struct {
	coordinator *sessionInputCoordinator
	key         string
	entry       *sessionInputEntry
	owner       bool
	previous    *sessionInputEntry
}

func sessionInputKey(store session.Store, id string) (string, error) {
	ns, err := session.SessionInputStoreIdentity(store)
	return ns + "\x00" + id, err
}

// acquire must run before acquiring runtime/session-operation locks. Failed
// owners wake waiters to retry election rather than caching failures.
func (c *sessionInputCoordinator) acquire(ctx context.Context, store session.Store, id string, binding sessionInputBinding) (*sessionInputTicket, error) {
	key, err := sessionInputKey(store, id)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		e := c.entries[key]
		if e == nil {
			e = &sessionInputEntry{binding: binding, done: make(chan struct{})}
			c.entries[key] = e
			c.mu.Unlock()
			return &sessionInputTicket{coordinator: c, key: key, entry: e, owner: true}, nil
		}
		if e.binding != binding {
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: session input binding changed; explicitly rebind the session before resuming", errServeSessionBusy)
		}
		if e.selection != nil {
			c.mu.Unlock()
			return &sessionInputTicket{coordinator: c, key: key, entry: e}, nil
		}
		done := e.done
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
func (t *sessionInputTicket) current() bool {
	c := t.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[t.key] == t.entry
}
func (t *sessionInputTicket) selected() *sessionInputSelection { return t.entry.selection }
func (t *sessionInputTicket) finish(pair sessionInputSelection) *sessionInputSelection {
	c := t.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	if !t.owner || c.entries[t.key] != t.entry {
		return t.entry.selection
	}
	intern := func(value string) string {
		if s, ok := c.strings[value]; ok {
			return s
		}
		c.strings[value] = value
		return value
	}
	pair.BasePrompt = intern(pair.BasePrompt)
	pair.Prompt = intern(pair.Prompt)
	pair.Tools = intern(pair.Tools)
	p := c.pairs[pair]
	if p == nil {
		p = &pair
		c.pairs[pair] = p
	}
	t.entry.selection = p
	t.owner = false
	close(t.entry.done)
	return p
}
func (t *sessionInputTicket) fail() {
	if t == nil {
		return
	}
	c := t.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.owner && c.entries[t.key] == t.entry {
		delete(c.entries, t.key)
		if t.previous != nil {
			c.entries[t.key] = t.previous
		}
		close(t.entry.done)
	}
	t.owner = false
}
func (c *sessionInputCoordinator) invalidate(store session.Store, id string) {
	key, err := sessionInputKey(store, id)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil {
		delete(c.entries, key)
		if e.selection == nil {
			close(e.done)
		}
	}
}
func equalSessionTools(a, b string) bool {
	canonical := func(s string) []string {
		names := tools.ParseToolsFlag(s)
		slices.Sort(names)
		return slices.Compact(names)
	}
	return slices.Equal(canonical(a), canonical(b))
}
func inputBinding(agent, dir string) sessionInputBinding {
	return sessionInputBinding{Agent: strings.TrimSpace(agent), Dir: dir}
}

func (c *sessionInputCoordinator) ready(store session.Store, id string) *sessionInputSelection {
	key, err := sessionInputKey(store, id)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil {
		return e.selection
	}
	return nil
}

type serveRuntimeRequest struct {
	settings                                      *SessionSettings
	agentSkills                                   *string
	approvalMode                                  *tools.ApprovalMode
	fresh                                         bool
	SessionID, Provider, Model, Agent, RuntimeDir string
	RefreshInputs                                 bool
	Inputs                                        *sessionInputSelection
}

// A deliberate fresh-history replacement stages a new selection but keeps the
// former ready entry available for rollback until durable preparation succeeds.
func (c *sessionInputCoordinator) acquireFresh(ctx context.Context, store session.Store, id string, binding sessionInputBinding) (*sessionInputTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := sessionInputKey(store, id)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	previous := c.entries[key]
	if previous != nil && previous.selection == nil {
		c.mu.Unlock()
		return c.acquire(ctx, store, id, binding)
	}
	e := &sessionInputEntry{binding: binding, done: make(chan struct{})}
	c.entries[key] = e
	c.mu.Unlock()
	return &sessionInputTicket{coordinator: c, key: key, entry: e, owner: true, previous: previous}, nil
}

// registerOwnedSessionInputs is called only after explicit owning-surface
// creation/rebinding succeeds. It performs no refresh transaction or reset.
func registerOwnedSessionInputs(store session.Store, sess *session.Session, pair sessionInputSelection) {
	if sess == nil {
		return
	}
	if _, supported := session.AsSessionInputRefresher(store); !supported {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	persisted, err := store.Get(ctx, sess.ID)
	if err != nil || persisted == nil {
		return
	}
	binding := inputBinding(sess.Agent, effectiveSessionDirectory(sess))
	key, err := sessionInputKey(store, sess.ID)
	if err != nil {
		return
	}
	c := processSessionInputs
	c.mu.Lock()
	e := c.entries[key]
	if e != nil && e.selection != nil && e.binding == binding && e.selection.Prompt == pair.Prompt && equalSessionTools(e.selection.Tools, pair.Tools) {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	ticket, err := c.acquireFresh(ctx, store, sess.ID, binding)
	if err != nil {
		return
	}
	defer ticket.fail()
	if ticket.owner {
		ticket.finish(pair)
	}
}

// Compatibility access never elects a refresh owner. After a session has been
// selected, however, a metadata-warmed runtime must not bypass that selection.
func runtimeHasSelectedInputs(rt *serveRuntime, selection *sessionInputSelection) bool {
	return selection == nil || (rt != nil && rt.inputs.Load() == selection)
}
