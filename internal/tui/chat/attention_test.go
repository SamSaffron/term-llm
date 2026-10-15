package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	sessionsui "github.com/samsaffron/term-llm/internal/tui/sessions"
	worktreesui "github.com/samsaffron/term-llm/internal/tui/worktrees"
)

type attentionAckCall struct {
	sessionID, storeID string
	seq                int64
}

type recordingAttentionStore struct {
	session.AttentionStore
	calls []attentionAckCall
	err   error
}

func (s *recordingAttentionStore) MarkAttentionSeenFrom(_ context.Context, sessionID, storeID string, seq int64, source string) (session.AttentionState, error) {
	if source != session.AttentionSourceTUI {
		return session.AttentionState{}, fmt.Errorf("unexpected attention source %q", source)
	}
	s.calls = append(s.calls, attentionAckCall{sessionID, storeID, seq})
	return session.AttentionState{}, s.err
}

func attentionTestFinalized(store session.AttentionStore, terminalAt time.Time, seq int64) attentionFinalizedMsg {
	return attentionFinalizedMsg{
		sessionID: "displayed",
		store:     store,
		state: session.AttentionState{
			StoreInstanceID: "store-instance", LatestAttentionSeq: seq,
			TerminalAt: terminalAt, Changed: true,
		},
	}
}

func assertAttentionAck(t *testing.T, store *recordingAttentionStore, seq int64) {
	t.Helper()
	want := attentionAckCall{"displayed", "store-instance", seq}
	if len(store.calls) != 1 || store.calls[0] != want {
		t.Fatalf("ack calls = %+v, want [%+v]", store.calls, want)
	}
}

func TestAttentionFinalizePresence(t *testing.T) {
	key := tea.KeyPressMsg{Code: 'x', Text: "x"}
	for _, tc := range []struct {
		name   string
		events []tea.Msg
		idle   time.Duration
		ack    bool
	}{
		{"focused recent key", []tea.Msg{tea.FocusMsg{}, key}, 10 * time.Second, true},
		{"focus unknown recent key", []tea.Msg{key}, 10 * time.Second, true},
		{"focus unknown lead boundary", []tea.Msg{key}, attentionPresenceLead, true},
		{"focus unknown idle", []tea.Msg{key}, attentionPresenceLead + time.Nanosecond, false},
		{"no presence", nil, 0, false},
		{"blurred recent key", []tea.Msg{tea.FocusMsg{}, tea.BlurMsg{}, key}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestChatModel(false)
			m.sess = &session.Session{ID: "displayed"}
			now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			m.attentionNow = func() time.Time { return now }
			for _, event := range tc.events {
				m.Update(event)
			}
			now = now.Add(tc.idle)
			store := &recordingAttentionStore{}
			_, cmd := m.Update(attentionTestFinalized(store, now, 42))
			if len(store.calls) != 0 {
				t.Fatal("Update acknowledged on the UI goroutine")
			}
			if tc.ack {
				if cmd == nil {
					t.Fatal("missing immediate acknowledgement command")
				}
				cmd()
				assertAttentionAck(t, store, 42)
				if m.pendingAttention != nil {
					t.Fatal("immediate ack left pending watermark")
				}
			} else if cmd != nil || m.pendingAttention == nil {
				t.Fatalf("expected pending ack: command=%v pending=%+v", cmd != nil, m.pendingAttention)
			}
		})
	}
}

func TestAttentionPendingPresence(t *testing.T) {
	key := tea.KeyPressMsg{Code: 'x', Text: "x"}
	mouse := tea.MouseClickMsg{Button: tea.MouseLeft}
	for _, tc := range []struct {
		name      string
		blurred   bool
		mouseMode bool
		event     tea.Msg
		ack       bool
	}{
		{"blurred key stays pending", true, false, key, false},
		{"blurred mouse stays pending", true, true, mouse, false},
		{"focus gains acknowledge", true, false, tea.FocusMsg{}, true},
		{"idle key acknowledges", false, false, key, true},
		{"enabled mouse acknowledges", false, true, mouse, true},
		{"disabled mouse ignored", false, false, mouse, false},
		{"resize ignored", false, false, tea.WindowSizeMsg{Width: 80, Height: 24}, false},
		{"blur ignored", false, false, tea.BlurMsg{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestChatModel(false)
			m.sess = &session.Session{ID: "displayed"}
			m.mouseMode = tc.mouseMode
			now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			m.attentionNow = func() time.Time { return now }
			if tc.blurred {
				m.Update(tea.BlurMsg{})
			}
			store := &recordingAttentionStore{}
			m.Update(attentionTestFinalized(store, now, 73))
			// Move far beyond completion: pending acks need fresh presence, not
			// the original finalization's sixty-second eligibility window.
			now = now.Add(time.Hour)
			cmd := m.updateAttention(tc.event)
			if len(store.calls) != 0 {
				t.Fatal("presence acknowledged on the UI goroutine")
			}
			if tc.ack {
				if cmd == nil {
					t.Fatal("presence did not return an ack command")
				}
				cmd()
				assertAttentionAck(t, store, 73)
				if m.updateAttention(tc.event) != nil {
					t.Fatal("presence acknowledged the same pending seq twice")
				}
			} else if cmd != nil || m.pendingAttention == nil {
				t.Fatalf("expected pending ack: command=%v pending=%+v", cmd != nil, m.pendingAttention)
			}
		})
	}
}

func TestAttentionPendingKeyCmdSurvivesInputRouting(t *testing.T) {
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "displayed"}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.attentionNow = func() time.Time { return now }
	store := &recordingAttentionStore{err: errors.New("best effort failure")}
	m.Update(attentionTestFinalized(store, now, 19))
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if cmd == nil || len(store.calls) != 0 {
		t.Fatal("key did not schedule an asynchronous ack")
	}
	// Update batches the ack last. Do not run unrelated textarea blink timers.
	if batch, ok := cmd().(tea.BatchMsg); ok {
		batch[len(batch)-1]()
	}
	assertAttentionAck(t, store, 19)
	if m.pendingAttention != nil || m.err != nil {
		t.Fatal("best-effort failure changed chat state or retained pending ack")
	}
}

func TestAttentionWaitsWhileFullScreenViewHidesTranscript(t *testing.T) {
	key := tea.KeyPressMsg{Code: 'x', Text: "x"}
	for _, tc := range []struct {
		name string
		hide func(*Model)
	}{
		{"inspector", func(m *Model) { m.inspectorMode = true }},
		{"worktree browser", func(m *Model) { m.worktreeBrowserMode = true }},
		{"resume browser", func(m *Model) { m.resumeBrowserMode = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestChatModel(false)
			m.sess = &session.Session{ID: "displayed"}
			now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			m.attentionNow = func() time.Time { return now }
			tc.hide(m)
			if m.updateAttention(key) != nil {
				t.Fatal("presence in a hidden view acknowledged")
			}
			store := &recordingAttentionStore{}
			if cmd := m.updateAttention(attentionTestFinalized(store, now, 31)); cmd != nil {
				t.Fatal("finalize acknowledged while the transcript was hidden")
			}
			if m.updateAttention(key) != nil || m.pendingAttention == nil {
				t.Fatal("hidden-view key consumed the pending acknowledgement")
			}
			m.inspectorMode, m.worktreeBrowserMode, m.resumeBrowserMode = false, false, false
			cmd := m.updateAttention(key)
			if cmd == nil {
				t.Fatal("presence after returning to the transcript did not acknowledge")
			}
			cmd()
			assertAttentionAck(t, store, 31)
		})
	}
}

func TestAttentionTransientAckFailureRetries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"transient", errors.New("database is locked"), true},
		{"conflict", session.ErrAttentionConflict, false},
		{"not found", session.ErrNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestChatModel(false)
			m.sess = &session.Session{ID: "displayed"}
			now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			m.attentionNow = func() time.Time { return now }
			store := &recordingAttentionStore{err: tc.err}
			m.updateAttention(attentionTestFinalized(store, now, 12))
			cmd := m.updateAttention(tea.KeyPressMsg{Code: 'x', Text: "x"})
			if cmd == nil {
				t.Fatal("missing ack command")
			}
			result := cmd()
			if failed, ok := result.(attentionAckFailedMsg); ok != tc.retry {
				t.Fatalf("result = %#v, want retry %v", result, tc.retry)
			} else if ok {
				m.updateAttention(failed)
			}
			if (m.pendingAttention != nil) != tc.retry {
				t.Fatalf("pending = %+v, want retry %v", m.pendingAttention, tc.retry)
			}
		})
	}
}

func TestAttentionPendingReplaced(t *testing.T) {
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "displayed"}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.attentionNow = func() time.Time { return now }
	store := &recordingAttentionStore{}
	for _, seq := range []int64{11, 12, 11} {
		m.Update(attentionTestFinalized(store, now, seq))
	}
	cmd := m.updateAttention(tea.FocusMsg{})
	if cmd == nil {
		t.Fatal("missing pending ack command")
	}
	cmd()
	assertAttentionAck(t, store, 12)
}

func TestAttentionPendingDroppedOnSessionSwitch(t *testing.T) {
	for _, mode := range []string{"new", "replacement", "loaded", "changed ID"} {
		t.Run(mode, func(t *testing.T) {
			m := newTestChatModel(false)
			m.sess = &session.Session{ID: "displayed"}
			store := &recordingAttentionStore{}
			m.Update(attentionTestFinalized(store, time.Now(), 55))
			switch mode {
			case "new":
				m.cmdNew()
			case "replacement":
				next := newTestChatModel(false)
				next.sess = &session.Session{ID: "other"}
				m.focusKnown, m.focused = true, false
				m.lastPresenceAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
				updated, _ := m.Update(sessionSwitchedMsg{model: next})
				if !next.focusKnown || next.focused || !next.lastPresenceAt.Equal(m.lastPresenceAt) {
					t.Fatal("replacement lost terminal presence/focus state")
				}
				m = updated.(*Model)
			case "loaded":
				m.Update(sessionLoadedMsg{sess: &session.Session{ID: "other"}})
			case "changed ID":
				m.sess = &session.Session{ID: "other"}
			}
			m.Update(tea.FocusMsg{})
			if m.pendingAttention != nil || len(store.calls) != 0 {
				t.Fatal("switched session retained or acknowledged old watermark")
			}
			// Navigating back does not resurrect the discarded watermark.
			m.sess = &session.Session{ID: "displayed"}
			if cmd := m.updateAttention(tea.FocusMsg{}); cmd != nil {
				t.Fatal("returning to session resurrected pending ack")
			}
		})
	}
}

func TestAttentionFinalizeIgnored(t *testing.T) {
	for _, mode := range []string{"other session", "unchanged", "no store", "transition", "quitting"} {
		t.Run(mode, func(t *testing.T) {
			m := newTestChatModel(false)
			m.sess = &session.Session{ID: "displayed"}
			store := &recordingAttentionStore{}
			msg := attentionTestFinalized(store, time.Now(), 8)
			switch mode {
			case "other session":
				msg.sessionID = "other"
			case "unchanged":
				msg.state.Changed = false
			case "no store":
				msg.store = nil
			case "transition":
				m.sessionTransition = &sessionTransition{}
			case "quitting":
				m.quitting = true
			}
			if cmd := m.updateAttention(msg); cmd != nil || m.pendingAttention != nil {
				t.Fatal("non-visible/unchanged finalization scheduled an ack")
			}
		})
	}
}

func TestAttentionPendingSurvivesTerminalHandoff(t *testing.T) {
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "displayed"}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.attentionNow = func() time.Time { return now }
	m.Update(tea.FocusMsg{})
	m.externalProcessActive = true
	store := &recordingAttentionStore{}
	if cmd := m.updateAttention(attentionTestFinalized(store, now, 66)); cmd != nil || m.pendingAttention == nil {
		t.Fatal("handoff did not retain the unseen outcome")
	}
	if cmd := m.updateAttention(tea.FocusMsg{}); cmd != nil || m.pendingAttention == nil {
		t.Fatal("presence during handoff acknowledged or dropped the outcome")
	}
	m.externalProcessActive = false
	cmd := m.updateAttention(tea.FocusMsg{})
	if cmd == nil {
		t.Fatal("presence after handoff did not acknowledge pending outcome")
	}
	cmd()
	assertAttentionAck(t, store, 66)
}

func TestChatViewReportsFocus(t *testing.T) {
	for _, mode := range []string{"inline", "alt screen", "auto send", "inspector", "sessions", "worktrees", "external", "quitting"} {
		t.Run(mode, func(t *testing.T) {
			m := newTestChatModel(mode == "alt screen")
			switch mode {
			case "auto send":
				m.autoSendQueue = []string{"hello"}
			case "inspector":
				m.inspectorMode = true
				m.inspectorModel = inspector.New(nil, 80, 24, m.styles)
			case "sessions":
				m.resumeBrowserMode = true
				m.resumeBrowserModel = sessionsui.New(nil, 80, 24, m.styles)
			case "worktrees":
				m.worktreeBrowserMode = true
				m.worktreeBrowserModel = worktreesui.New("", nil, "", 80, 24, m.styles)
			case "external":
				m.externalProcessActive = true
			case "quitting":
				m.quitting = true
			}
			want := mode != "external" && mode != "quitting"
			if got := m.View().ReportFocus; got != want {
				t.Fatalf("ReportFocus = %v, want %v", got, want)
			}
		})
	}
}

type attentionLifecycleStore struct {
	session.ServeResponseLifecycleStore
	state         session.AttentionState
	err           error
	finalizations int
}

func (s *attentionLifecycleStore) FinalizeResponseRun(context.Context, session.ResponseRunTerminal) (session.AttentionState, error) {
	s.finalizations++
	return s.state, s.err
}

func TestTurnLeaseReleaseReturnsAttentionWithoutAcknowledging(t *testing.T) {
	for _, tc := range []struct {
		name      string
		changed   bool
		withStore bool
		err       error
		want      bool
	}{
		{"changed", true, true, nil, true},
		{"unchanged", false, true, nil, false},
		{"no attention store", true, false, nil, false},
		{"finalization error", true, true, errors.New("unreachable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingAttentionStore{}
			state := attentionTestFinalized(store, time.Now(), 87).state
			state.Changed = tc.changed
			lifecycle := &attentionLifecycleStore{state: state, err: tc.err}
			lease := &turnLease{store: lifecycle, sessionID: "displayed", stop: make(chan struct{})}
			if tc.withStore {
				lease.attention = store
			}
			msg := lease.release(session.ResponseRunCompleted)
			if (msg != nil) != tc.want {
				t.Fatalf("finalized message = %+v, want non-nil=%v", msg, tc.want)
			}
			if msg != nil && (msg.state != state || msg.sessionID != "displayed" || msg.store != store) {
				t.Fatalf("finalized message lost captured state: %+v", msg)
			}
			if len(store.calls) != 0 {
				t.Fatal("lease release acknowledged attention without UI presence")
			}
			if lease.release(session.ResponseRunCompleted) != nil || lifecycle.finalizations != 1 {
				t.Fatal("releasing twice emitted/finalized more than once")
			}
		})
	}
}

type attentionDeliveryProbe struct {
	model    *Model
	received bool
}

func (p *attentionDeliveryProbe) Init() tea.Cmd  { return nil }
func (p *attentionDeliveryProbe) View() tea.View { return tea.NewView("") }
func (p *attentionDeliveryProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(attentionFinalizedMsg); ok {
		p.received = true
		_, cmd := p.model.Update(msg)
		return p, tea.Sequence(cmd, tea.Quit)
	}
	return p, nil
}

func TestTurnLeaseAttentionDeliveredToModel(t *testing.T) {
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "displayed"}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.attentionNow = func() time.Time { return now }
	m.Update(tea.FocusMsg{})
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	store := &recordingAttentionStore{}
	lifecycle := &attentionLifecycleStore{state: attentionTestFinalized(store, now, 91).state}
	lease := &turnLease{store: lifecycle, attention: store, sessionID: "displayed", stop: make(chan struct{})}
	m.activeTurnLease.Store(lease)
	probe := &attentionDeliveryProbe{model: m}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	program := tea.NewProgram(probe, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithWindowSize(80, 24), tea.WithEnvironment([]string{"TERM=xterm-256color"}), tea.WithoutSignalHandler())
	m.SetProgram(program)
	// This path is also called on Update: release must not block on Send while
	// the event loop is busy (here, not yet started).
	m.releaseTurnLease(lease, session.ResponseRunCompleted)
	if len(store.calls) != 0 || m.activeTurnLease.Load() != nil {
		t.Fatal("release acknowledged directly or retained the lease")
	}
	if _, err := program.Run(); err != nil {
		t.Fatal(err)
	}
	if !probe.received {
		t.Fatal("finalization did not reach the model")
	}
	assertAttentionAck(t, store, 91)
}
