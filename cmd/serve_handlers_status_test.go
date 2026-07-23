package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestHandleSessionsStatusTranscriptUpdatedAtChangesOnMessageUpdate(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	createdAt := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	sess := &session.Session{
		ID:        "sess-status-transcript",
		Summary:   "status transcript test",
		Provider:  "test",
		Model:     "test-model",
		Mode:      session.ModeChat,
		Origin:    session.OriginWeb,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := session.NewMessage(sess.ID, llm.AssistantText("initial partial answer"), -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	first := readStatusEntryForSession(t, store, sess.ID)
	if first.TranscriptRev <= 0 {
		t.Fatalf("transcript_rev = %d, want positive", first.TranscriptRev)
	}
	if first.TranscriptUpdatedAt <= 0 {
		t.Fatalf("transcript_updated_at = %d, want positive", first.TranscriptUpdatedAt)
	}
	if first.MsgCount != 1 {
		t.Fatalf("message_count after add = %d, want 1", first.MsgCount)
	}
	if first.LastMessageAt <= 0 {
		t.Fatalf("last_message_at after add = %d, want positive", first.LastMessageAt)
	}

	time.Sleep(5 * time.Millisecond)
	updated := session.NewMessage(sess.ID, llm.AssistantText("updated partial answer"), msg.Sequence)
	updated.ID = msg.ID
	updated.CreatedAt = msg.CreatedAt
	if err := store.UpdateMessage(ctx, sess.ID, updated); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}

	second := readStatusEntryForSession(t, store, sess.ID)
	if second.TranscriptRev <= first.TranscriptRev {
		t.Fatalf("transcript_rev did not advance after UpdateMessage: before=%d after=%d", first.TranscriptRev, second.TranscriptRev)
	}
	if second.TranscriptUpdatedAt <= first.TranscriptUpdatedAt {
		t.Fatalf("transcript_updated_at did not advance after UpdateMessage: before=%d after=%d", first.TranscriptUpdatedAt, second.TranscriptUpdatedAt)
	}
	if second.MsgCount != first.MsgCount {
		t.Fatalf("message_count changed after UpdateMessage: before=%d after=%d", first.MsgCount, second.MsgCount)
	}
	if second.LastMessageAt != first.LastMessageAt {
		t.Fatalf("last_message_at changed after UpdateMessage: before=%d after=%d", first.LastMessageAt, second.LastMessageAt)
	}
}

type countingStatusTranscriptStore struct {
	session.Store
	indexer  session.TranscriptIndexer
	revCalls int
}

func (s *countingStatusTranscriptStore) GetTranscriptIndex(ctx context.Context, sessionID string) (int64, []session.TranscriptIndexItem, error) {
	return s.indexer.GetTranscriptIndex(ctx, sessionID)
}

func (s *countingStatusTranscriptStore) GetTranscriptSnapshot(ctx context.Context, sessionID string) (session.TranscriptSnapshot, error) {
	return s.indexer.GetTranscriptSnapshot(ctx, sessionID)
}

func (s *countingStatusTranscriptStore) GetMessagesByTranscriptRanges(ctx context.Context, sessionID string, ranges []session.TranscriptRange) (int64, []session.Message, error) {
	return s.indexer.GetMessagesByTranscriptRanges(ctx, sessionID, ranges)
}

func (s *countingStatusTranscriptStore) TranscriptRev(ctx context.Context, sessionID string) (int64, error) {
	s.revCalls++
	return s.indexer.TranscriptRev(ctx, sessionID)
}

func (s *countingStatusTranscriptStore) TranscriptVersioned() bool { return true }
func (s *countingStatusTranscriptStore) SessionSummariesIncludeTranscriptRev() bool {
	return true
}

func TestHandleSessionsStatusUsesListTranscriptRevisionsWithoutNPlusOne(t *testing.T) {
	ctx := context.Background()
	base, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer base.Close()
	indexer := base.(session.TranscriptIndexer)
	for i := 0; i < 3; i++ {
		sess := &session.Session{ID: fmt.Sprintf("status-list-%d", i), Provider: "test", Model: "test", Mode: session.ModeChat}
		if err := base.Create(ctx, sess); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		if err := base.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.UserText("hello"), -1)); err != nil {
			t.Fatalf("AddMessage %d: %v", i, err)
		}
	}
	counting := &countingStatusTranscriptStore{Store: base, indexer: indexer}
	logged := session.NewLoggingStore(counting, nil)
	srv := &serveServer{store: logged}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionsStatus(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if counting.revCalls != 0 {
		t.Fatalf("TranscriptRev called %d times; list summaries should supply revisions", counting.revCalls)
	}
	var payload struct {
		Sessions []sessionsStatusTestEntry `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sessions) != 3 {
		t.Fatalf("sessions=%d, want 3", len(payload.Sessions))
	}
	for _, entry := range payload.Sessions {
		if entry.TranscriptRev <= 0 {
			t.Fatalf("session %s transcript_rev=%d", entry.ID, entry.TranscriptRev)
		}
	}
}

func TestSessionMessagesETagIgnoresSessionMetadataOnlyUpdates(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sess := &session.Session{
		ID:        "sess-messages-etag",
		Summary:   "messages etag test",
		Provider:  "test",
		Model:     "test-model",
		Mode:      session.ModeChat,
		Origin:    session.OriginWeb,
		CreatedAt: time.Now().Add(-time.Hour).Truncate(time.Millisecond),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.AssistantText("same visible transcript"), -1)); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-messages-etag/messages?tail=1&limit=200", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("initial messages status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("initial messages response missing ETag")
	}

	// Metadata/usage updates bump sessions.updated_at, which is intentionally the
	// broad transcript marker used by /v1/sessions/status. The messages endpoint
	// ETag should stay content-based so such over-approximate status changes do
	// not force a full transcript body and UI re-render when the visible transcript
	// response is unchanged.
	time.Sleep(5 * time.Millisecond)
	if err := store.UpdateMetrics(ctx, sess.ID, 1, 0, 10, 5, 0, 0); err != nil {
		t.Fatalf("UpdateMetrics: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-messages-etag/messages?tail=1&limit=200", nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("conditional messages status after metadata-only update = %d, want 304; body=%s", rr.Code, rr.Body.String())
	}
}

type sessionsStatusTestEntry struct {
	ID                  string `json:"id"`
	MsgCount            int    `json:"message_count"`
	LastMessageAt       int64  `json:"last_message_at"`
	TranscriptRev       int64  `json:"transcript_rev"`
	TranscriptUpdatedAt int64  `json:"transcript_updated_at"`
}

func readStatusEntryForSession(t *testing.T, store session.Store, sessionID string) sessionsStatusTestEntry {
	t.Helper()

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionsStatus(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var payload struct {
		Sessions []sessionsStatusTestEntry `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status response: %v; body=%s", err, rr.Body.String())
	}
	for _, entry := range payload.Sessions {
		if entry.ID == sessionID {
			return entry
		}
	}
	t.Fatalf("session %q missing from status response: %s", sessionID, rr.Body.String())
	return sessionsStatusTestEntry{}
}
