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

func TestHandleSessionsStatusActivityChangesOnMessageUpdate(t *testing.T) {
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
		Pinned:    true,
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
	if !first.Pinned {
		t.Fatal("pinned status was not projected")
	}
	if first.Number <= 0 {
		t.Fatalf("number = %d, want canonical positive session number", first.Number)
	}
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
	if second.LastMessageAt <= first.LastMessageAt {
		t.Fatalf("last_message_at did not advance after UpdateMessage: before=%d after=%d", first.LastMessageAt, second.LastMessageAt)
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

func TestHandleSessionsStatusAlwaysIncludesSelectedActiveAndUnresolvedSessions(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Millisecond)
	ids := make([]string, 205)
	for i := range ids {
		ids[i] = fmt.Sprintf("status-window-%03d", i)
		if err := store.Create(ctx, &session.Session{
			ID: ids[i], Provider: "test", Model: "test", Mode: session.ModeChat,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	runs := newServeResponseRunManager()
	defer runs.Close()
	run := newResponseRun("response-outside-window", ids[0], "", "test", time.Now().Unix(), nil)
	if err := runs.create(run); err != nil {
		t.Fatal(err)
	}
	runs.setActiveRun(ids[0], run.id)
	manager := &serveSessionManager{sessions: map[string]*serveRuntime{
		ids[1]: {
			pendingAskUsers: map[string]*servePendingAskUser{
				"ask-outside-window": {CallID: "ask-outside-window", CreatedAt: time.Now()},
			},
		},
	}}
	srv := &serveServer{store: store, responseRuns: runs, sessionMgr: manager}
	req := httptest.NewRequest(
		http.MethodGet,
		"/v1/sessions/status?selected_session="+ids[2],
		nil,
	)
	rr := httptest.NewRecorder()
	srv.handleSessionsStatus(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Sessions []struct {
			ID                      string `json:"id"`
			ActiveRun               bool   `json:"active_run"`
			ActiveResponseID        string `json:"active_response_id"`
			InteractionRequired     bool   `json:"interaction_required"`
			PendingInteractionCount int    `json:"pending_interaction_count"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]struct {
		ActiveRun               bool
		ActiveResponseID        string
		InteractionRequired     bool
		PendingInteractionCount int
	}, len(payload.Sessions))
	for _, entry := range payload.Sessions {
		byID[entry.ID] = struct {
			ActiveRun               bool
			ActiveResponseID        string
			InteractionRequired     bool
			PendingInteractionCount int
		}{entry.ActiveRun, entry.ActiveResponseID, entry.InteractionRequired, entry.PendingInteractionCount}
	}
	if len(payload.Sessions) != 203 {
		t.Fatalf("sessions=%d, want bounded 200 plus three critical rows", len(payload.Sessions))
	}
	if got := byID[ids[0]]; !got.ActiveRun || got.ActiveResponseID != run.id {
		t.Fatalf("active session missing or inactive: %#v", got)
	}
	if got, ok := byID[ids[1]]; !ok {
		t.Fatal("unresolved interaction session outside window is missing")
	} else if !got.InteractionRequired || got.PendingInteractionCount != 1 {
		t.Fatalf("unresolved interaction state = %#v", got)
	}
	if _, ok := byID[ids[2]]; !ok {
		t.Fatal("selected session outside window is missing")
	}
}

// sessionsStatusEntryKeys is the documented wire shape of one status entry,
// pinned so the runningSessionIDs extraction cannot silently add or rename a
// projection field.
var sessionsStatusEntryKeys = map[string]bool{
	"id": true, "number": true, "project_id": true, "project_name": true,
	"short_title": true, "long_title": true, "pinned": true, "active_run": true,
	"active_response_id": true, "run_epoch": true, "started_rev": true, "started_at": true,
	"client_message_id": true, "anchor_row_id": true, "transcript_rev": true, "message_count": true,
	"last_message_at": true, "transcript_updated_at": true, "attention_store_instance_id": true,
	"attention_seq": true, "attention_response_id": true, "attention_final_rev": true,
	"seen_through_seq": true, "attention_unseen": true, "attention_outcome": true,
	"attention_terminal_at": true, "interaction_required": true, "interaction_response_id": true,
	"interaction_state_rev": true, "pending_interaction_count": true, "pending_interaction_kinds": true,
	"interaction_required_since": true,
}

// TestSessionsStatusRunningProjectionMatchesRunningSessionIDs guards the
// runningSessionIDs extraction (shared with the voice session directory): the
// handler output is unchanged, byte-identical across calls, marks exactly the
// sessions the shared primitive reports as running, and keeps every projection
// field of the documented shape.
func TestSessionsStatusRunningProjectionMatchesRunningSessionIDs(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	for i, id := range []string{"status-run-durable", "status-run-memory", "status-idle"} {
		if err := store.Create(ctx, &session.Session{
			ID: id, Provider: "test", Model: "test-model", Mode: session.ModeChat,
			CreatedAt: base.Add(time.Duration(i) * time.Minute), UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	// Durable running row owned by another process: reachable only through the
	// attention projection.
	lifecycle, ok := session.AsServeResponseLifecycleStore(store)
	if !ok {
		t.Fatal("store does not support durable response runs")
	}
	if _, err := lifecycle.AdmitResponseRun(ctx, session.ResponseRunAdmission{
		ResponseID: "durable-response", SessionID: "status-run-durable",
		RunEpoch: 1, OwnerInstanceID: "other-process", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	runs := newServeResponseRunManager()
	defer runs.Close()
	runs.setActiveRun("status-run-memory", "memory-response")
	srv := &serveServer{store: store, responseRuns: runs}

	read := func() (int, string, string) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handleSessionsStatus(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil))
		return rr.Code, rr.Body.String(), rr.Header().Get("ETag")
	}
	firstCode, firstBody, firstETag := read()
	if firstCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", firstCode, firstBody)
	}
	secondCode, secondBody, secondETag := read()
	if secondCode != http.StatusOK || firstBody != secondBody || firstETag != secondETag || firstETag == "" {
		t.Fatalf("status output is not stable: %d/%d etag %q/%q", firstCode, secondCode, firstETag, secondETag)
	}

	var payload struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(firstBody), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sessions) != 3 {
		t.Fatalf("sessions=%d body=%s", len(payload.Sessions), firstBody)
	}
	running := srv.runningSessionIDs(ctx)
	if len(running) != 2 || !running["status-run-durable"] || !running["status-run-memory"] {
		t.Fatalf("runningSessionIDs = %v", running)
	}
	listed := make(map[string]bool)
	for _, entry := range payload.Sessions {
		for key := range entry {
			if !sessionsStatusEntryKeys[key] {
				t.Fatalf("undocumented status field %q in %s", key, firstBody)
			}
		}
		var id string
		if err := json.Unmarshal(entry["id"], &id); err != nil {
			t.Fatal(err)
		}
		listed[id] = true
		for _, required := range []string{"id", "short_title", "long_title", "transcript_rev", "message_count", "last_message_at", "transcript_updated_at", "interaction_required"} {
			if _, ok := entry[required]; !ok {
				t.Fatalf("session %s lost required field %q: %s", id, required, entry[required])
			}
		}
		active := false
		if raw, ok := entry["active_run"]; ok {
			if err := json.Unmarshal(raw, &active); err != nil {
				t.Fatal(err)
			}
		}
		if active != running[id] {
			t.Fatalf("session %s active_run=%t, runningSessionIDs=%t", id, active, running[id])
		}
		if id == "status-run-memory" {
			var responseID string
			if err := json.Unmarshal(entry["active_response_id"], &responseID); err != nil || responseID != "memory-response" {
				t.Fatalf("local active response = %q, %v", responseID, err)
			}
		}
		if id == "status-run-durable" {
			if raw, ok := entry["active_response_id"]; ok {
				t.Fatalf("non-local durable response advertised a local stream: %s", raw)
			}
		}
	}
	for id := range running {
		if !listed[id] {
			t.Fatalf("running session %s missing from status output", id)
		}
	}
}

type sessionsStatusTestEntry struct {
	ID                  string `json:"id"`
	Number              int64  `json:"number"`
	Pinned              bool   `json:"pinned"`
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
