package cmd

import (
	"context"
	"fmt"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

// Bubble Tea can delay a Cmd after Update returns. Reserve ownership when the
// command is emitted, not when its goroutine eventually starts. Only passive
// presentation/event waiters opt out. Final program exit discards unstarted
// commands; reload never discards or cancels started work.
type chatCommandScope struct {
	mu     sync.Mutex
	closed bool
	leases map[*chatCommandLease]bool
}
type chatCommandLease struct {
	scope   *chatCommandScope
	release func()
}

func (s *chatCommandScope) reserve() *chatCommandLease {
	_, release, err := restart.Default.Activity(context.Background())
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		release()
		return nil
	}
	if s.leases == nil {
		s.leases = make(map[*chatCommandLease]bool)
	}
	lease := &chatCommandLease{s, release}
	s.leases[lease] = false
	return lease
}
func (l *chatCommandLease) start() bool {
	l.scope.mu.Lock()
	defer l.scope.mu.Unlock()
	if _, ok := l.scope.leases[l]; !ok || l.scope.closed {
		return false
	}
	l.scope.leases[l] = true
	return true
}
func (l *chatCommandLease) finish() {
	l.scope.mu.Lock()
	delete(l.scope.leases, l)
	l.scope.mu.Unlock()
	l.release()
}
func (s *chatCommandScope) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for lease, started := range s.leases {
		if !started {
			delete(s.leases, lease)
			lease.release()
		}
	}
}

func (m *chatProgramModel) scope(cmd tea.Cmd) tea.Cmd {
	if m.reloadCommands == nil {
		return scopeChatProgramCmd(cmd, m.generation)
	}
	return scopeChatProgramCmd(cmd, m.generation, m.reloadCommands)
}
func (m *chatProgramModel) syncReloadBusy() {
	if m.reloadCommands == nil {
		return
	}
	model, ok := m.model.(*chat.Model)
	if !ok {
		return
	}
	if model.ReloadBusy() {
		if m.reloadBusyRelease == nil {
			_, release, err := restart.Default.Activity(context.Background())
			if err == nil {
				m.reloadBusyRelease = release
			}
		}
	} else if m.reloadBusyRelease != nil {
		m.reloadBusyRelease()
		m.reloadBusyRelease = nil
	}
}

// TUI keeps its live model/resources through a failed exec. ReleaseTerminal is
// reversible; the old destructive quit/cleanup path is not used for SIGUSR2.
func bindChatReload(ctx context.Context, p *tea.Program, host *chatProgramModel, reporter chatLifecycleReporter) func() {
	bindCtx, cancel := context.WithCancel(ctx)
	host.reloadCtx = bindCtx
	var state chat.ReloadState
	unregister := restart.Default.Register(&restart.Resource{
		Check: func(ctx context.Context) error {
			reply := make(chan chat.ReloadInspection, 1)
			go p.Send(chat.ReloadInspectMsg{Context: ctx, Reply: reply})
			select {
			case result := <-reply:
				state = result.State
				return result.Err
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		Prepare: func(ctx context.Context) (func(context.Context), error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return process.SaveState("chat", state)
		},
	})
	bound := make(chan func(), 1)
	go func() {
		select {
		case <-bindCtx.Done():
			bound <- nil
			return
		case <-host.reloadReady:
		}
		stop, err := restart.Default.Bind(bindCtx, func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.ReleaseTerminal(); err != nil {
				return fmt.Errorf("release terminal: %w", err)
			}
			restoreLifecycleOSC(reporter)
			err := ctx.Err()
			if err == nil {
				err = execReload(state.SessionID)
			}
			restoreErr := p.RestoreTerminal()
			if restoreErr != nil {
				return fmt.Errorf("reload: %v; restore terminal: %w", err, restoreErr)
			}
			p.Send(chat.FooterNoticeMsg{Text: fmt.Sprintf("Reload failed; original session retained: %v", err)})
			return err
		})
		if err != nil {
			p.Send(chat.FooterNoticeMsg{Text: err.Error()})
		}
		bound <- stop
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			if stop := <-bound; stop != nil {
				stop()
			}
			unregister()
			host.reloadCommands.close()
			if host.reloadBusyRelease != nil {
				host.reloadBusyRelease()
				host.reloadBusyRelease = nil
			}
		})
	}
}
