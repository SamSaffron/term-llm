package cmd

import (
	"context"
	"encoding/json"
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
