package serve

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/notification"
	"github.com/samsaffron/term-llm/internal/session"
)

type notificationRecheckStore struct {
	*session.SQLiteStore
	beforeSubscriptionRead func()
}

func (s *notificationRecheckStore) GetPushSubscription(ctx context.Context, id string) (*session.PushSubscription, error) {
	if callback := s.beforeSubscriptionRead; callback != nil {
		s.beforeSubscriptionRead = nil
		callback()
	}
	return s.SQLiteStore.GetPushSubscription(ctx, id)
}

func TestCompletionDispatchRechecksReadAndPresence(t *testing.T) {
	for _, action := range []string{"read", "presence", "optout", "deliver"} {
		t.Run(action, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sessions.db")
			sqlite, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer sqlite.Close()
			store := &notificationRecheckStore{SQLiteStore: sqlite}
			cfg := &config.Config{}
			cfg.Serve.WebPush.VAPIDPublicKey = "test-key"
			cfg.Serve.WebPush.VAPIDPrivateKey = "test-private"
			s := &serveServer{store: store, cfgRef: cfg}
			sub, err := sqlite.UpsertPushSubscription(context.Background(), &session.PushSubscription{Endpoint: "https://example.invalid/not-used", KeyP256DH: "key", KeyAuth: "auth", VAPIDKeyID: appnotify.WebPushKeyID("test-key")})
			if err != nil {
				t.Fatal(err)
			}
			candidate := seedCompletionCandidate(t, s, sqlite, "final", "session", sub.ID)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE completion_push_outbox SET next_attempt_at='2000-01-01 00:00:00'`); err != nil {
				t.Fatal(err)
			}
			store.beforeSubscriptionRead = func() {
				switch action {
				case "read":
					_, err = sqlite.MarkAttentionSeen(context.Background(), candidate.SessionID, candidate.StoreInstanceID, candidate.AttentionSeq)
				case "presence":
					err = sqlite.UpdateNotificationPresence(context.Background(), session.NotificationPresence{Version: 2, WindowID: candidate.WindowID, DocumentID: candidate.DocumentID, SubscriptionID: sub.ID, Source: candidate.Source, Sequence: 2, Active: true, ExpiresAt: time.Now().Add(time.Minute)})
				case "optout":
					err = sqlite.DeletePushSubscriptionByID(context.Background(), sub.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			sends := 0
			s.dispatchCompletionPushesWithSender(context.Background(), nil, time.Second, func(_ context.Context, got *session.PushSubscription, payload []byte, opts *webpush.Options) (int, time.Duration, error) {
				sends++
				var envelope completionPushPayload
				if err := json.Unmarshal(payload, &envelope); err != nil {
					t.Fatal(err)
				}
				if got.ID != sub.ID || envelope.Version != 2 || envelope.Kind != "completion" || envelope.WindowID != candidate.WindowID || envelope.AttentionSeq != candidate.AttentionSeq || envelope.StoreInstanceID != candidate.StoreInstanceID || envelope.TurnID == "" {
					t.Fatalf("lost owner/read identity: %+v", envelope)
				}
				if opts.Topic != notification.Topic(envelope.EventID) || opts.RecordSize != 2048 {
					t.Fatalf("unstable topic/record: %+v", opts)
				}
				return http.StatusCreated, 0, nil
			})
			want := 0
			if action == "deliver" {
				want = 1
			}
			if sends != want {
				t.Fatalf("sends=%d, want%d", sends, want)
			}
			if action == "presence" {
				var status string
				var seen int64
				if err := db.QueryRow(`SELECT status FROM completion_push_outbox`).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT seen_through_seq FROM session_attention`).Scan(&seen); err != nil {
					t.Fatal(err)
				}
				if status != "pending" || seen != 0 {
					t.Fatalf("presence faked read/consumed event: status=%s seen=%d", status, seen)
				}
			}
		})
	}
}

func TestNotificationPresenceHTTPBindingAndFailClosedOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.Config{}
	cfg.Serve.WebPush.VAPIDPublicKey = "test-key"
	s := &serveServer{store: store, cfgRef: cfg}
	sub, err := store.UpsertPushSubscription(context.Background(), &session.PushSubscription{Endpoint: "https://example.invalid/enrolled", KeyP256DH: "key", KeyAuth: "auth", VAPIDKeyID: appnotify.WebPushKeyID("test-key")})
	if err != nil {
		t.Fatal(err)
	}
	p := session.NotificationPresence{Version: 2, Source: "/node/Studio", WindowID: "window_owner", DocumentID: "document_owner", SubscriptionID: sub.ID, Sequence: 1, Active: true, ExpiresAt: time.Now().Add(time.Minute)}
	send := func(p session.NotificationPresence) *httptest.ResponseRecorder {
		body, _ := json.Marshal(p)
		r := httptest.NewRequest(http.MethodPost, "/v1/push/presence", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.handleNotificationPresence(w, r)
		return w
	}
	if w := send(p); w.Code != 200 {
		t.Fatalf("presence status%d: %s", w.Code, w.Body.String())
	}
	p.DocumentID = "document_clone"
	p.Sequence = 2
	if w := send(p); w.Code != 409 {
		t.Fatalf("clone stole live window: %d", w.Code)
	}
	for _, source := range []string{"https://elsewhere.invalid/ui", "//elsewhere/ui", "/ui/../bad", "/ui/%2e%2e/bad", "/ui?x=1", "/ui#frag", "/ui%5cbad"} {
		if validNotificationSource(source) {
			t.Errorf("unsafe source accepted: %s", source)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("X-Term-LLM-UI-Version", "test")
	r.Header.Set(requestPushSubscriptionHeader, sub.ID)
	if origin := s.notificationOrigin(r); origin.SubscriptionID != "" {
		t.Fatal("legacy subscription acquired automatic authority")
	}
	r.Header.Set(requestNotificationVersionHeader, "2")
	r.Header.Set(requestNotificationWindowHeader, "window_owner")
	r.Header.Set(requestNotificationDocumentHeader, "document_owner")
	r.Header.Set(requestNotificationSourceHeader, "/node/Studio")
	if origin := s.notificationOrigin(r); origin.Source != "/node/Studio" || origin.WindowID != "window_owner" {
		t.Fatalf("lost public owner scope: %+v", origin)
	}
}
