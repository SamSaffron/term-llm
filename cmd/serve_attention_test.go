package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func attentionHandlerFixture(t *testing.T) (*serveServer, *session.SQLiteStore, string, session.AttentionState) {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	sessionID := session.NewID()
	if err := store.Create(ctx, &session.Session{ID: sessionID, Provider: "test", Model: "test", Origin: session.OriginWeb, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: "resp_attention", SessionID: sessionID, RunEpoch: 1, OwnerInstanceID: "owner", StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.FinalizeResponseRun(ctx, session.ResponseRunTerminal{ResponseID: "resp_attention", OwnerInstanceID: "owner", FencingToken: lease.FencingToken, Outcome: session.ResponseRunCompleted, FinalRev: 3})
	if err != nil {
		t.Fatal(err)
	}
	return &serveServer{store: store}, store, sessionID, state
}

func TestSessionAttentionSeenHandlerRequiresExactStoreAndSequence(t *testing.T) {
	srv, _, sessionID, state := attentionHandlerFixture(t)
	request := func(body any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/attention/seen", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handleSessionAttentionSeen(rr, req, sessionID)
		return rr
	}
	if rr := request(map[string]any{"store_instance_id": state.StoreInstanceID, "through_seq": state.LatestAttentionSeq + 1}); rr.Code != http.StatusConflict {
		t.Fatalf("over-latest status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if rr := request(map[string]any{"store_instance_id": "store_replaced", "through_seq": state.LatestAttentionSeq}); rr.Code != http.StatusConflict {
		t.Fatalf("store mismatch status = %d", rr.Code)
	}
	rr := request(map[string]any{"store_instance_id": state.StoreInstanceID, "through_seq": state.LatestAttentionSeq})
	if rr.Code != http.StatusOK {
		t.Fatalf("seen status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var response session.AttentionState
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Unseen || response.SeenThroughSeq != state.LatestAttentionSeq {
		t.Fatalf("seen response = %+v", response)
	}
}

type conflictOnceAttentionStore struct {
	session.Store
	attention session.AttentionStore
	calls     int
}

func (s *conflictOnceAttentionStore) MarkAttentionSeen(ctx context.Context, sessionID, storeID string, seq int64) (session.AttentionState, error) {
	return s.attention.MarkAttentionSeen(ctx, sessionID, storeID, seq)
}
func (s *conflictOnceAttentionStore) GetAttention(ctx context.Context, sessionID string) (session.AttentionState, error) {
	return s.attention.GetAttention(ctx, sessionID)
}
func (s *conflictOnceAttentionStore) ListAttention(ctx context.Context, opts session.AttentionListOptions) (session.AttentionPage, error) {
	s.calls++
	if s.calls == 1 {
		return session.AttentionPage{}, session.ErrAttentionConflict
	}
	return s.attention.ListAttention(ctx, opts)
}
func (s *conflictOnceAttentionStore) StoreInstanceID(ctx context.Context) (string, error) {
	return s.attention.StoreInstanceID(ctx)
}

func TestSessionsStatusRetriesDurableRunningSnapshotConflict(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sessionID := session.NewID()
	if err := store.Create(ctx, &session.Session{ID: sessionID, Provider: "test", Model: "test", Origin: session.OriginWeb, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: "running-response", SessionID: sessionID,
		RunEpoch: 1, OwnerInstanceID: "other-process", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	wrapped := &conflictOnceAttentionStore{Store: store, attention: store}
	srv := &serveServer{store: wrapped}
	recorder := httptest.NewRecorder()
	srv.handleSessionsStatus(recorder, httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil))
	if recorder.Code != http.StatusOK || wrapped.calls < 2 || !bytes.Contains(recorder.Body.Bytes(), []byte(`"active_run":true`)) {
		t.Fatalf("conflict retry status=%d calls=%d body=%s", recorder.Code, wrapped.calls, recorder.Body.String())
	}
}

func TestSessionsStatusDoesNotAdvertiseNonLocalDurableResponseStream(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sessionID := session.NewID()
	if err := store.Create(ctx, &session.Session{ID: sessionID, Provider: "test", Model: "test", Origin: session.OriginWeb, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: "remote-response", SessionID: sessionID,
		RunEpoch: 1, OwnerInstanceID: "other-process", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: store}
	recorder := httptest.NewRecorder()
	srv.handleSessionsStatus(recorder, httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Sessions []struct {
			ID               string `json:"id"`
			ActiveRun        bool   `json:"active_run"`
			ActiveResponseID string `json:"active_response_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Sessions) != 1 || !payload.Sessions[0].ActiveRun || payload.Sessions[0].ActiveResponseID != "" {
		t.Fatalf("durable non-local status = %+v", payload.Sessions)
	}
}

func TestAttentionHandlersReportUnsupportedStore(t *testing.T) {
	srv := &serveServer{}
	snapshot := httptest.NewRecorder()
	srv.handleAttention(snapshot, httptest.NewRequest(http.MethodGet, "/v1/attention?kind=unseen", nil))
	if snapshot.Code != http.StatusNotImplemented {
		t.Fatalf("snapshot status = %d, body=%s", snapshot.Code, snapshot.Body.String())
	}
	seen := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/session/attention/seen", bytes.NewBufferString(`{"store_instance_id":"store","through_seq":1}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleSessionAttentionSeen(seen, req, "session")
	if seen.Code != http.StatusNotImplemented {
		t.Fatalf("seen status = %d, body=%s", seen.Code, seen.Body.String())
	}
}

func TestSelectedWebSessionIncludesAttentionVisitGate(t *testing.T) {
	srv, _, sessionID, state := attentionHandlerFixture(t)
	selected, err := srv.selectedWebSession(context.Background(), sessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || selected.AttentionStoreInstanceID != state.StoreInstanceID ||
		selected.AttentionSeq != state.LatestAttentionSeq || selected.AttentionFinalRev != 3 || !selected.AttentionUnseen {
		t.Fatalf("selected attention projection = %+v, marker=%+v", selected, state)
	}
}

func TestAttentionSnapshotHandlerETagAndNoTranscriptContent(t *testing.T) {
	srv, _, _, _ := attentionHandlerFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/attention?kind=unseen&limit=1", nil)
	rr := httptest.NewRecorder()
	srv.handleAttention(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("messages")) || bytes.Contains(rr.Body.Bytes(), []byte("transcript_content")) {
		t.Fatalf("snapshot leaked transcript fields: %s", rr.Body.String())
	}
	revalidate := httptest.NewRequest(http.MethodGet, "/v1/attention?kind=unseen&limit=1", nil)
	revalidate.Header.Set("If-None-Match", rr.Header().Get("ETag"))
	notModified := httptest.NewRecorder()
	srv.handleAttention(notModified, revalidate)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("revalidation status = %d", notModified.Code)
	}
}
