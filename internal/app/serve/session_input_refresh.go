package serve

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// Immutable selections contain no runtime, provider, manager, or store references.
type SessionInputSelection struct{ BasePrompt, Prompt, Tools string }
type SessionInputBinding struct{ Agent, Dir string }
type sessionInputEntry struct {
	binding   SessionInputBinding
	done      chan struct{}
	selection *SessionInputSelection
}
type SessionInputCoordinator struct {
	mu      sync.Mutex
	entries map[string]*sessionInputEntry
	pairs   map[SessionInputSelection]*SessionInputSelection
	strings map[string]string
}

func NewSessionInputCoordinator() *SessionInputCoordinator {
	return &SessionInputCoordinator{entries: make(map[string]*sessionInputEntry), pairs: make(map[SessionInputSelection]*SessionInputSelection), strings: make(map[string]string)}
}

var processSessionInputs = NewSessionInputCoordinator()

type SessionInputTicket struct {
	coordinator *SessionInputCoordinator
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
func (c *SessionInputCoordinator) Acquire(ctx context.Context, store session.Store, id string, binding SessionInputBinding) (*SessionInputTicket, error) {
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
			return &SessionInputTicket{coordinator: c, key: key, entry: e, owner: true}, nil
		}
		if e.binding != binding {
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: session input binding changed; explicitly rebind the session before resuming", errServeSessionBusy)
		}
		if e.selection != nil {
			c.mu.Unlock()
			return &SessionInputTicket{coordinator: c, key: key, entry: e}, nil
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
func (t *SessionInputTicket) current() bool {
	c := t.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[t.key] == t.entry
}
func (t *SessionInputTicket) Selected() *SessionInputSelection { return t.entry.selection }
func (t *SessionInputTicket) Finish(pair SessionInputSelection) *SessionInputSelection {
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
func (t *SessionInputTicket) Fail() {
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
func (c *SessionInputCoordinator) Invalidate(store session.Store, id string) {
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
func EqualSessionTools(a, b string) bool {
	canonical := func(s string) []string {
		names := tools.ParseToolsFlag(s)
		slices.Sort(names)
		return slices.Compact(names)
	}
	return slices.Equal(canonical(a), canonical(b))
}
func InputBinding(agent, dir string) SessionInputBinding {
	return SessionInputBinding{Agent: strings.TrimSpace(agent), Dir: dir}
}

func (c *SessionInputCoordinator) Ready(store session.Store, id string) *SessionInputSelection {
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
	settings     *SessionSettings
	agentSkills  *string
	approvalMode *tools.ApprovalMode
	fresh        bool
	// swapCandidate marks the runtime a model swap is installing. Such a
	// runtime deliberately runs a provider the session row does not name yet,
	// so it must not be "corrected" back before its first turn persists.
	swapCandidate                                 bool
	SessionID, Provider, Model, Agent, RuntimeDir string
	RefreshInputs                                 bool
	Inputs                                        *SessionInputSelection
}

// A deliberate fresh-history replacement stages a new selection but keeps the
// former ready entry available for rollback until durable preparation succeeds.
func (c *SessionInputCoordinator) AcquireFresh(ctx context.Context, store session.Store, id string, binding SessionInputBinding) (*SessionInputTicket, error) {
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
		return c.Acquire(ctx, store, id, binding)
	}
	e := &sessionInputEntry{binding: binding, done: make(chan struct{})}
	c.entries[key] = e
	c.mu.Unlock()
	return &SessionInputTicket{coordinator: c, key: key, entry: e, owner: true, previous: previous}, nil
}

// registerOwnedSessionInputs is called only after explicit owning-surface
// creation/rebinding succeeds. It performs no refresh transaction or reset.
func RegisterOwnedSessionInputs(store session.Store, sess *session.Session, pair SessionInputSelection) {
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
	binding := InputBinding(sess.Agent, appruntime.EffectiveSessionDirectory(sess))
	key, err := sessionInputKey(store, sess.ID)
	if err != nil {
		return
	}
	c := ProcessSessionInputs()
	c.mu.Lock()
	e := c.entries[key]
	if e != nil && e.selection != nil && e.binding == binding && e.selection.Prompt == pair.Prompt && EqualSessionTools(e.selection.Tools, pair.Tools) {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	ticket, err := c.AcquireFresh(ctx, store, sess.ID, binding)
	if err != nil {
		return
	}
	defer ticket.Fail()
	if ticket.owner {
		ticket.Finish(pair)
	}
}

// Compatibility access never elects a refresh owner. After a session has been
// selected, however, a metadata-warmed runtime must not bypass that selection.
func runtimeHasSelectedInputs(rt *serveRuntime, selection *SessionInputSelection) bool {
	return selection == nil || (rt != nil && rt.inputs.Load() == selection)
}

// ProcessSessionInputs returns the single process-owned input coordinator.
func ProcessSessionInputs() *SessionInputCoordinator { return processSessionInputs }

// SwapProcessSessionInputs temporarily replaces the process coordinator and returns
// an exact-pointer restore function. Callers must serialize swaps with its users.
func SwapProcessSessionInputs(c *SessionInputCoordinator) func() {
	previous := processSessionInputs
	processSessionInputs = c
	return func() { processSessionInputs = previous }
}

// Owner reports whether this ticket owns selection preparation.
func (t *SessionInputTicket) Owner() bool { return t.owner }
