package sessions

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/session"
)

type embeddedTestStore struct {
	session.NoopStore
	summaries []session.SessionSummary
}

func (s *embeddedTestStore) List(_ context.Context, _ session.ListOptions) ([]session.SessionSummary, error) {
	return s.summaries, nil
}

func TestEmbeddedQuitEmitsCloseMsg(t *testing.T) {
	m := New(nil, 80, 24, nil)
	m.SetEmbedded(true)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("expected quit key to return a command")
	}
	msg := cmd()
	if _, ok := msg.(CloseMsg); !ok {
		t.Fatalf("expected CloseMsg from embedded quit, got %T", msg)
	}
}

func TestSetPreferredSessionIDSelectsMatchingSessionOnRefresh(t *testing.T) {
	store := &embeddedTestStore{
		summaries: []session.SessionSummary{
			{ID: "sess-1", Number: 1, UpdatedAt: time.Now().Add(-time.Hour)},
			{ID: "sess-2", Number: 2, UpdatedAt: time.Now()},
		},
	}
	m := New(store, 80, 24, nil)
	m.SetPreferredSessionID("sess-1")

	updated, _ := m.Update(RefreshMsg{})
	browser := updated.(*Model)
	if browser.cursor != 1 {
		t.Fatalf("expected preferred session cursor to be 1 after sort, got %d", browser.cursor)
	}
}

func TestView_EmbeddedModeUsesResumeCopy(t *testing.T) {
	m := New(nil, 80, 24, nil)
	m.SetEmbedded(true)
	m.sessions = []session.SessionSummary{{
		ID:        "sess-1",
		Number:    1,
		Name:      "draft ideas",
		Summary:   "explored resume browser layout",
		UpdatedAt: time.Now(),
	}}

	out := m.View()
	if !strings.Contains(out, "Resume Session") {
		t.Fatalf("expected embedded header in view, got %q", out)
	}
	if !strings.Contains(out, "[enter] resume") {
		t.Fatalf("expected embedded help copy in view, got %q", out)
	}
}
