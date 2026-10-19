package shell

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/restart"
)

type Manager struct {
	ttl    time.Duration
	exists func(string) bool

	mu         sync.Mutex
	shells     map[string]*Shell
	operations map[string]*sync.Mutex
	closed     bool
	stopCh     chan struct{}
	closeCh    sync.Once
}

func NewManager(ttl time.Duration, exists func(string) bool) *Manager {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	m := &Manager{ttl: ttl, exists: exists, shells: make(map[string]*Shell), operations: make(map[string]*sync.Mutex), stopCh: make(chan struct{})}
	go m.janitor()
	return m
}

func (m *Manager) janitor() {
	interval := m.ttl / 2
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.evictExpired()
		case <-m.stopCh:
			return
		}
	}
}

func (m *Manager) evictExpired() {
	now := time.Now()
	m.mu.Lock()
	candidates := make(map[string]*Shell, len(m.shells))
	for sessionID, shell := range m.shells {
		candidates[sessionID] = shell
	}
	m.mu.Unlock()

	for sessionID, shell := range candidates {
		op := m.sessionOperation(sessionID)
		op.Lock()
		missing := m.exists != nil && !m.exists(sessionID)
		if !missing && now.Sub(shell.idleSince()) <= m.ttl {
			op.Unlock()
			continue
		}
		m.mu.Lock()
		current := m.shells[sessionID] == shell
		m.mu.Unlock()
		if current {
			// Invalidate authority while this generation is still discoverable. A
			// response that pins collaboration in this interval must observe either
			// the live shared generation or its explicit exited state, never a
			// transient missing entry that could be interpreted as local mode.
			shell.invalidate(-1)
			m.mu.Lock()
			if m.shells[sessionID] == shell {
				delete(m.shells, sessionID)
			}
			m.mu.Unlock()
			// Keep this session's operation serialized until the old PTY is closed;
			// otherwise create could publish a second live generation in the gap.
			shell.close()
		}
		op.Unlock()
	}
}

func (m *Manager) sessionOperation(sessionID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	op := m.operations[sessionID]
	if op == nil {
		op = &sync.Mutex{}
		m.operations[sessionID] = op
	}
	return op
}

func (m *Manager) Create(sessionID, cwd string, cols, rows int) (*Shell, bool, error) {
	op := m.sessionOperation(sessionID)
	op.Lock()
	defer op.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, false, ErrClosed
	}
	current := m.shells[sessionID]
	m.mu.Unlock()
	if current != nil && current.Alive() {
		if err := current.Resize(cols, rows); err != nil {
			return nil, false, fmt.Errorf("resize attached shell: %w", err)
		}
		return current, false, nil
	}
	if current != nil {
		m.mu.Lock()
		if m.shells[sessionID] == current {
			delete(m.shells, sessionID)
		}
		m.mu.Unlock()
		current.close()
	}
	workCtx, release, reloadErr := restart.Default.Root(context.Background())
	if reloadErr != nil {
		return nil, false, reloadErr
	}
	defer release()
	id, err := newShellID()
	if err != nil {
		return nil, false, err
	}
	shell := NewShell(id, sessionID, cwd)
	process, err := startProcess(cwd, cols, rows, shell.AppendOutput)
	if err != nil {
		shell.generationCancel()
		return nil, false, err
	}
	shell.process = process
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		shell.close()
		return nil, false, ErrClosed
	}
	m.shells[sessionID] = shell
	m.mu.Unlock()
	_ = restart.Default.Go(workCtx, func(context.Context) { shell.watchExit() })
	return shell, true, nil
}

func newShellID() (string, error) {
	var value [18]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate shell ID: %w", err)
	}
	return "sh_" + base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func (m *Manager) Get(sessionID, shellID string) (*Shell, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	shell := m.shells[sessionID]
	if shell == nil || shell.id != shellID {
		return nil, ErrStale
	}
	shell.touch()
	return shell, nil
}

func (m *Manager) CloseShell(sessionID, shellID string) error {
	op := m.sessionOperation(sessionID)
	op.Lock()
	defer op.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	shell := m.shells[sessionID]
	if shell == nil || shell.id != shellID {
		m.mu.Unlock()
		return ErrStale
	}
	m.mu.Unlock()
	shell.invalidate(-1)
	m.mu.Lock()
	if m.shells[sessionID] == shell {
		delete(m.shells, sessionID)
	}
	m.mu.Unlock()
	shell.close()
	return nil
}

func (m *Manager) CloseSession(sessionID string) {
	op := m.sessionOperation(sessionID)
	op.Lock()
	defer op.Unlock()
	m.mu.Lock()
	shell := m.shells[sessionID]
	m.mu.Unlock()
	if shell != nil {
		shell.invalidate(-1)
		m.mu.Lock()
		if m.shells[sessionID] == shell {
			delete(m.shells, sessionID)
		}
		m.mu.Unlock()
		shell.close()
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.closeCh.Do(func() { close(m.stopCh) })
	type shellTarget struct {
		sessionID string
		shell     *Shell
	}
	targets := make([]shellTarget, 0, len(m.shells))
	for sessionID, shell := range m.shells {
		targets = append(targets, shellTarget{sessionID: sessionID, shell: shell})
	}
	// Keep generations discoverable to the controller until their authority is
	// invalidated. Creation is already blocked by closed. Per-session operation
	// locks prevent shutdown from racing another close/eviction of the same PTY.
	m.mu.Unlock()
	for _, target := range targets {
		op := m.sessionOperation(target.sessionID)
		op.Lock()
		m.mu.Lock()
		current := m.shells[target.sessionID] == target.shell
		m.mu.Unlock()
		if current {
			target.shell.invalidate(-1)
			m.mu.Lock()
			if m.shells[target.sessionID] == target.shell {
				delete(m.shells, target.sessionID)
			}
			m.mu.Unlock()
			target.shell.close()
		}
		op.Unlock()
	}
	m.mu.Lock()
	m.shells = map[string]*Shell{}
	m.mu.Unlock()
}

// Adopt publishes an already constructed shell as the session's current
// generation. It is intended for tests that supply a fake Process.
func (m *Manager) Adopt(sessionID string, shell *Shell) {
	m.mu.Lock()
	m.shells[sessionID] = shell
	m.mu.Unlock()
}

// Closed reports whether the manager has been shut down.
func (m *Manager) Closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}
