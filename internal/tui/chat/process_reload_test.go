package chat

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestProcessReloadIdlePreservesDraft(t *testing.T) {
	m := newTestChatModel(false)
	m.setTextareaValue("unsent draft")
	updated, cmd := m.Update(ProcessReloadMsg{})
	m = updated.(*Model)
	if cmd == nil || !m.WantsReload() {
		t.Fatal("idle signal did not use reload")
	}
	if state := m.ProcessReloadState(); state.Draft != "unsent draft" || state.Continue {
		t.Fatalf("state: %+v", state)
	}
	if _, cmd := m.Update(ProcessReloadMsg{}); cmd != nil {
		t.Fatal("duplicate reload was not coalesced")
	}
}

func TestProcessReloadGraceUsesNormalCancellation(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	cancelled := 0
	m.streamCancelFunc = func() { cancelled++ }
	_, _ = m.Update(ProcessReloadMsg{})
	if cancelled != 0 {
		t.Fatal("cancelled before grace expired")
	}
	m.processReload.started = time.Now().Add(-11 * time.Second)
	_, _ = m.Update(processReloadTick{})
	if cancelled != 1 || !m.processReload.state.Continue {
		t.Fatal("grace did not cancel and retain continuation")
	}
	if !m.engine.SteeringTransitioning() {
		t.Fatal("dispatch was not frozen")
	}
	if err := m.engine.FreezeExecution(llm.SteeringTransition{OperationID: "another", Fence: 2}); err == nil {
		t.Fatal("second owner entered")
	}
	m.engine.ReleaseSteeringFreeze(m.processReload.owner, false)
}

func TestProcessReloadUserStopCancelsRestart(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.streamCancelFunc = func() {}
	_, _ = m.Update(ProcessReloadMsg{})
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.processReload.pending || m.processReload.state.Continue {
		t.Fatal("user Stop retained automatic restart")
	}
}

func TestProcessResumeAddsNoFakeUserTurn(t *testing.T) {
	m := newTestChatModel(true)
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "session.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m.store = store
	m.sess = &session.Session{ID: "resume-test", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.Create(context.Background(), m.sess); err != nil {
		t.Fatal(err)
	}
	m.SetProcessReloadState(ProcessReloadState{Draft: "preserve me", Continue: true})
	users := m.sess.UserTurns
	_, _ = m.resumeAfterProcessReload()
	if m.sess.UserTurns != users {
		t.Fatal("internal resume counted as user input")
	}
	if m.textarea.Value() != "preserve me" {
		t.Fatal("continuation cleared draft")
	}
	rows, err := m.store.GetMessages(context.Background(), m.SessionID(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Role == llm.RoleDeveloper {
			found = true
		}
	}
	if !found {
		t.Fatal("missing durable internal recovery event")
	}
}

func TestProcessReloadStoppedAfterGraceReleasesFenceOnlyAfterSettlement(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.streamCancelFunc = func() {}
	m.processReload = processReload{pending: true, started: time.Now().Add(-11 * time.Second)}
	_, _ = m.handleProcessReload()
	owner := m.processReload.owner
	if owner.OperationID == "" {
		t.Fatal("grace did not freeze engine")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	_, _ = m.handleProcessReload()
	if m.processReload.owner.OperationID == "" {
		t.Fatal("released fence before stream settled")
	}
	m.streaming = false
	_, _ = m.handleProcessReload()
	if m.processReload.owner.OperationID != "" {
		t.Fatal("settled Stop left stale fence")
	}
	next := llm.SteeringTransition{OperationID: "next-restart", Fence: 2}
	if err := m.engine.FreezeExecution(next); err != nil {
		t.Fatal("engine remains unusable after Stop", err)
	}
	m.engine.ReleaseSteeringFreeze(next, false)
}
