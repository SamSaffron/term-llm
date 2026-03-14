package chat

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestAllCommandsIncludesCompact(t *testing.T) {
	commands := AllCommands()
	found := false
	for _, cmd := range commands {
		if cmd.Name == "compact" {
			found = true
			if cmd.Usage != "/compact" {
				t.Errorf("compact usage = %q, want /compact", cmd.Usage)
			}
			break
		}
	}
	if !found {
		t.Error("AllCommands() should include 'compact' command")
	}
}

func TestFilterCommandsMatchesCompact(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"compact", true},
		{"comp", true},
		{"compa", true},
		{"xyz", false},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			results := FilterCommands(tt.query)
			found := false
			for _, cmd := range results {
				if cmd.Name == "compact" {
					found = true
					break
				}
			}
			if found != tt.want {
				t.Errorf("FilterCommands(%q) found compact = %v, want %v", tt.query, found, tt.want)
			}
		})
	}
}

func TestAllCommandsRemovesLoadAndKeepsResume(t *testing.T) {
	commands := AllCommands()

	hasLoad := false
	hasResume := false
	for _, cmd := range commands {
		if cmd.Name == "load" {
			hasLoad = true
		}
		if cmd.Name == "resume" {
			hasResume = true
		}
	}

	if hasLoad {
		t.Error("AllCommands() should not include 'load'")
	}
	if !hasResume {
		t.Error("AllCommands() should include 'resume'")
	}
}

// mockStore implements session.Store for testing resume behavior.
type mockStore struct {
	session.NoopStore
	sessions  map[string]*session.Session
	messages  map[string][]session.Message
	summaries []session.SessionSummary
	msgErr    error
	updated   *session.Session
	updateErr error
}

func (s *mockStore) Get(_ context.Context, id string) (*session.Session, error) {
	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}
	return nil, nil
}

func (s *mockStore) GetByPrefix(_ context.Context, prefix string) (*session.Session, error) {
	if sess, ok := s.sessions[prefix]; ok {
		return sess, nil
	}
	return nil, nil
}

func (s *mockStore) GetMessages(_ context.Context, sessionID string, _, _ int) ([]session.Message, error) {
	if s.msgErr != nil {
		return nil, s.msgErr
	}
	return s.messages[sessionID], nil
}

func (s *mockStore) List(_ context.Context, _ session.ListOptions) ([]session.SessionSummary, error) {
	return s.summaries, nil
}

func (s *mockStore) Update(_ context.Context, sess *session.Session) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updated = sess
	return nil
}

// newCmdTestModel creates a minimal Model suitable for testing command functions.
func newCmdTestModel(store session.Store) *Model {
	ta := textarea.New()
	return &Model{
		width:    80,
		height:   24,
		textarea: ta,
		dialog:   NewDialogModel(nil),
		store:    store,
	}
}

func TestCmdResume_DirectResumeRequestsRelaunch(t *testing.T) {
	sessionID := "sess-resume-1"
	sess := &session.Session{ID: sessionID, Number: 1, Name: "my session"}
	msgs := []session.Message{
		{ID: 1, SessionID: sessionID, Role: "user", TextContent: "hello"},
		{ID: 2, SessionID: sessionID, Role: "assistant", TextContent: "hi"},
	}
	store := &mockStore{
		sessions: map[string]*session.Session{sessionID: sess},
		messages: map[string][]session.Message{sessionID: msgs},
	}
	m := newCmdTestModel(store)
	result, _ := m.cmdResume([]string{sessionID})
	rm := result.(*Model)

	if !rm.quitting {
		t.Fatal("expected cmdResume to request chat relaunch via quit")
	}
	if rm.RequestedResumeSessionID() != sessionID {
		t.Fatalf("expected pending resume session ID %q, got %q", sessionID, rm.RequestedResumeSessionID())
	}
	if len(rm.messages) != 0 {
		t.Errorf("expected no in-place message load, got %d messages", len(rm.messages))
	}
	if len(msgs) == 0 {
		t.Fatal("test fixture expected non-empty source messages")
	}
}

func TestCmdResume_DoesNotMutateViewStateInPlace(t *testing.T) {
	sessionID := "sess-cache-bug"
	sess := &session.Session{ID: sessionID, Number: 2}
	msgs := []session.Message{
		{ID: 1, SessionID: sessionID},
		{ID: 2, SessionID: sessionID},
	}
	store := &mockStore{
		sessions: map[string]*session.Session{sessionID: sess},
		messages: map[string][]session.Message{sessionID: msgs},
	}
	m := newCmdTestModel(store)

	// Simulate stale cache with the same message count as the incoming session.
	// Before the fix, historyValid stays true because the count check is a false positive.
	m.viewCache.historyValid = true
	m.viewCache.historyMsgCount = len(msgs) // same count — the bug
	m.viewCache.contentVersion = 5

	result, _ := m.cmdResume([]string{sessionID})
	rm := result.(*Model)

	if !rm.viewCache.historyValid {
		t.Error("expected in-place view cache to be untouched because resume now relaunches chat")
	}
	if rm.viewCache.contentVersion != 5 {
		t.Errorf("expected contentVersion to remain unchanged, got %d", rm.viewCache.contentVersion)
	}
	if rm.RequestedResumeSessionID() != sessionID {
		t.Fatalf("expected pending resume session ID %q, got %q", sessionID, rm.RequestedResumeSessionID())
	}
}

func TestCmdResume_NoArgs_OpensEmbeddedSessionsBrowser(t *testing.T) {
	sessionID := "sess-resume-picker-1"
	store := &mockStore{
		summaries: []session.SessionSummary{
			{
				ID:           sessionID,
				Number:       7,
				Name:         "session seven",
				MessageCount: 3,
				Model:        "claude-sonnet-20250101",
				Summary:      "Discussed release notes and rollout checks",
				UpdatedAt:    time.Now().Add(-5 * time.Minute),
			},
		},
	}
	m := newCmdTestModel(store)
	m.setTextareaValue("draft note")

	result, _ := m.cmdResume([]string{})
	rm := result.(*Model)

	if !rm.resumeBrowserMode {
		t.Fatal("expected resume browser mode to be active")
	}
	if rm.resumeBrowserModel == nil {
		t.Fatal("expected embedded resume browser model to be initialized")
	}
	if rm.dialog.IsOpen() {
		t.Fatal("expected generic dialog to remain closed")
	}
	if got := rm.textarea.Value(); got != "draft note" {
		t.Fatalf("expected draft input to be preserved, got %q", got)
	}
}

func TestSwitchModel_UpdatesSessionMetadata(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{
		ID:       "sess-model-switch-1",
		Provider: "OpenAI (old-model)",
		Model:    "old-model",
	}
	m.providerName = "OpenAI (old-model)"
	m.modelName = "old-model"
	m.engine = llm.NewEngine(llm.NewMockProvider("old"), nil)

	result, _ := m.switchModel("debug:fast")
	rm := result.(*Model)

	if rm.sess.Provider != "debug:fast" {
		t.Fatalf("expected session provider to be updated to %q, got %q", "debug:fast", rm.sess.Provider)
	}
	if rm.sess.Model != "fast" {
		t.Fatalf("expected session model to be updated to %q, got %q", "fast", rm.sess.Model)
	}
	if store.updated == nil {
		t.Fatal("expected switchModel to persist session changes")
	}
}

func TestResumeFormatAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"just now", now.Add(-30 * time.Second), "just now"},
		{"minutes", now.Add(-5 * time.Minute), "5m ago"},
		{"hours", now.Add(-2 * time.Hour), "2h ago"},
		{"days", now.Add(-2 * 24 * time.Hour), "2d ago"},
		{"old date", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), "Jan 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resumeFormatAge(tt.t)
			if got != tt.want {
				t.Errorf("resumeFormatAge() = %q, want %q", got, tt.want)
			}
		})
	}
}
