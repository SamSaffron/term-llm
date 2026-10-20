package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

type resumeStore struct {
	session.NoopStore
	current   *session.Session
	byPrefix  map[string]*session.Session
	byID      map[string]*session.Session
	summaries []session.SessionSummary

	currentErr error
	listErr    error
	getErr     error
	prefixErr  error
}

func (s *resumeStore) GetCurrent(_ context.Context) (*session.Session, error) {
	if s.currentErr != nil {
		return nil, s.currentErr
	}
	return s.current, nil
}

func (s *resumeStore) List(_ context.Context, _ session.ListOptions) ([]session.SessionSummary, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.summaries, nil
}

func (s *resumeStore) Get(_ context.Context, id string) (*session.Session, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.byID[id], nil
}

func (s *resumeStore) GetByPrefix(_ context.Context, prefix string) (*session.Session, error) {
	if s.prefixErr != nil {
		return nil, s.prefixErr
	}
	return s.byPrefix[prefix], nil
}

func TestResolveChatResumeSession_ExplicitPrefix(t *testing.T) {
	sess := &session.Session{ID: "sess-explicit-1"}
	store := &resumeStore{
		byPrefix: map[string]*session.Session{"abc": sess},
	}

	got, err := resolveChatResumeSession(context.Background(), store, "abc")
	if err != nil {
		t.Fatalf("resolveChatResumeSession() error = %v", err)
	}
	if got != sess {
		t.Fatalf("resolveChatResumeSession() returned wrong session: got %v want %v", got, sess)
	}
}

func TestResolveChatResumeSession_UsesCurrentWhenNoID(t *testing.T) {
	sess := &session.Session{ID: "sess-current-1"}
	store := &resumeStore{current: sess}

	got, err := resolveChatResumeSession(context.Background(), store, "")
	if err != nil {
		t.Fatalf("resolveChatResumeSession() error = %v", err)
	}
	if got != sess {
		t.Fatalf("resolveChatResumeSession() returned wrong session: got %v want %v", got, sess)
	}
}

func TestResolveChatResumeSession_FallsBackToMostRecentSummary(t *testing.T) {
	sess := &session.Session{ID: "sess-summary-1"}
	store := &resumeStore{
		currentErr: errors.New("not available"),
		byID:       map[string]*session.Session{sess.ID: sess},
		summaries: []session.SessionSummary{
			{ID: sess.ID},
		},
	}

	got, err := resolveChatResumeSession(context.Background(), store, "")
	if err != nil {
		t.Fatalf("resolveChatResumeSession() error = %v", err)
	}
	if got != sess {
		t.Fatalf("resolveChatResumeSession() returned wrong session: got %v want %v", got, sess)
	}
}

func TestResolveChatResumeSession_NoSession(t *testing.T) {
	store := &resumeStore{
		currentErr: errors.New("no current"),
		summaries:  []session.SessionSummary{},
	}

	_, err := resolveChatResumeSession(context.Background(), store, "")
	if err == nil {
		t.Fatal("expected error when no resume session exists")
	}
}
