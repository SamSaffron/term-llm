package serve

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	appnotify "github.com/samsaffron/term-llm/internal/app/notify"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestCompletionPushTimeoutDoesNotStarveOutbox(t *testing.T) {
	for _, exhaustBatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("exhaustBatch=%t", exhaustBatch), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sessions.db")
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			cfg := &config.Config{}
			cfg.Serve.WebPush.VAPIDPublicKey = "test-key"
			cfg.Serve.WebPush.VAPIDPrivateKey = "test-private-key"
			s := &serveServer{store: store, cfgRef: cfg}
			for _, name := range []string{"slow", "healthy"} {
				sub, err := store.UpsertPushSubscription(context.Background(), &session.PushSubscription{
					Endpoint: "https://push.example/" + name, KeyP256DH: "key", KeyAuth: "auth",
					VAPIDKeyID: appnotify.WebPushKeyID(cfg.Serve.WebPush.VAPIDPublicKey),
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.EnqueueCompletionPush(context.Background(), session.CompletionPushOutboxItem{
					EventID: name, ResponseID: name, SubscriptionID: sub.ID, Payload: []byte(name),
				}); err != nil {
					t.Fatal(err)
				}
			}
			assertRow := func(name, wantStatus string, wantAttempts int) string {
				t.Helper()
				var status, next string
				var attempts int
				if err := db.QueryRow(`SELECT status, attempt_count, next_attempt_at FROM completion_push_outbox WHERE event_id = ?`, name).Scan(&status, &attempts, &next); err != nil {
					t.Fatal(err)
				}
				if status != wantStatus || attempts != wantAttempts {
					t.Fatalf("%s: status=%s attempts=%d; want %s/%d", name, status, attempts, wantStatus, wantAttempts)
				}
				return next
			}
			var calls []string
			send := func(ctx context.Context, _ *session.PushSubscription, payload []byte, _ *webpush.Options) (int, time.Duration, error) {
				calls = append(calls, string(payload))
				if string(payload) == "slow" {
					<-ctx.Done()
					return 0, 0, fmt.Errorf("send: %w", ctx.Err())
				}
				if err := ctx.Err(); err != nil {
					t.Fatalf("healthy send received expired context: %v", err)
				}
				return http.StatusCreated, 0, nil
			}
			ctx := context.Background()
			attemptTimeout := time.Millisecond
			if exhaustBatch {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
				attemptTimeout = time.Second
			}
			before := assertRow("slow", "pending", 0)
			s.dispatchCompletionPushesWithSender(ctx, store, attemptTimeout, send)
			next := assertRow("slow", "pending", 1)
			if next <= before {
				t.Fatalf("retry did not advance next_attempt_at: %q -> %q", before, next)
			}
			if len(calls) == 0 || calls[0] != "slow" {
				t.Fatalf("oldest row was not attempted first: %v", calls)
			}
			if exhaustBatch {
				assertRow("healthy", "pending", 0)
				if len(calls) != 1 {
					t.Fatalf("continued exhausted batch: %v", calls)
				}
				s.dispatchCompletionPushesWithSender(context.Background(), store, time.Millisecond, send)
			}
			assertRow("healthy", "delivered", 1)

			// Advance eligibility directly instead of waiting through exponential backoff.
			// Preserve the existing cutoff: seven retries, then the eighth failure is dead.
			for attempt := 2; attempt <= 8; attempt++ {
				if _, err := db.Exec(`UPDATE completion_push_outbox SET next_attempt_at = '2000-01-01 00:00:00' WHERE event_id = 'slow'`); err != nil {
					t.Fatal(err)
				}
				s.dispatchCompletionPushesWithSender(context.Background(), store, time.Millisecond, send)
				status := "pending"
				if attempt == 8 {
					status = "dead"
				}
				assertRow("slow", status, attempt)
			}
			assertRow("healthy", "delivered", 1)
		})
	}
}

func TestCompletionPushTitleIdentifiesNode(t *testing.T) {
	cases := []struct {
		name    string
		cfg     serveServerConfig
		outcome string
		want    string
	}{
		{"no node identity keeps original wording", serveServerConfig{}, "completed", "Response complete"},
		{"ui title labels the node", serveServerConfig{uiTitle: "workshop"}, "completed", "workshop: Response complete"},
		{"hub node name when no title", serveServerConfig{hubNodeName: "artist"}, "completed", "artist: Response complete"},
		{"hub node id as last resort", serveServerConfig{hubNodeID: "node-7"}, "completed", "node-7: Response complete"},
		{"title wins over hub fields", serveServerConfig{uiTitle: "Studio", hubNodeName: "artist"}, "completed", "Studio: Response complete"},
		{"blank title falls through", serveServerConfig{uiTitle: "   ", hubNodeName: "artist"}, "completed", "artist: Response complete"},
		{"failures are labelled too", serveServerConfig{uiTitle: "workshop"}, "failed", "workshop: Response failed"},
		{"failures without identity are unchanged", serveServerConfig{}, "failed", "Response failed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sessions.db")
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			sub, err := store.UpsertPushSubscription(context.Background(), &session.PushSubscription{
				Endpoint: "https://push.example/node", KeyP256DH: "key", KeyAuth: "auth",
			})
			if err != nil {
				t.Fatal(err)
			}

			s := &serveServer{store: store, cfg: tc.cfg}
			s.enqueueCompletionPush("resp-1", "sess-1", sub.ID, tc.outcome, time.Now())

			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			var raw []byte
			if err := db.QueryRow(`SELECT payload FROM completion_push_outbox WHERE response_id = ?`, "resp-1").Scan(&raw); err != nil {
				t.Fatalf("no push enqueued: %v", err)
			}
			var payload completionPushPayload
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Title != tc.want {
				t.Errorf("title = %q, want %q", payload.Title, tc.want)
			}
			// The body is intentionally left alone; only the title gains the label.
			if payload.Body == "" {
				t.Error("body should not be empty")
			}
		})
	}
}

func TestCompletionPushRecordSize(t *testing.T) {
	for _, tt := range []struct {
		name       string
		oversized  bool
		httpStatus int
		rejectAll  bool
	}{
		{name: "success", httpStatus: http.StatusCreated},
		{name: "local payload too large", oversized: true, httpStatus: http.StatusCreated},
		{name: "relay payload too large", httpStatus: http.StatusRequestEntityTooLarge},
		{name: "relay rejects fallback", httpStatus: http.StatusRequestEntityTooLarge, rejectAll: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := requests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read push body: %v", err)
				}
				if tt.oversized || request > 1 {
					if len(body) >= appnotify.WebPushRecordSize {
						t.Errorf("fallback body length = %d, want less than %d", len(body), appnotify.WebPushRecordSize)
					}
				} else if len(body) != appnotify.WebPushRecordSize {
					t.Errorf("push body length = %d, want %d", len(body), appnotify.WebPushRecordSize)
				}
				if got := r.Header.Get("TTL"); got != "300" {
					t.Errorf("TTL = %q, want 300", got)
				}
				if request > 1 && !tt.rejectAll {
					w.WriteHeader(http.StatusCreated)
				} else {
					w.WriteHeader(tt.httpStatus)
				}
			}))
			defer endpoint.Close()
			browserKey, err := ecdh.P256().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "sessions.db")
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cfg := &config.Config{}
			cfg.Serve.WebPush.VAPIDPublicKey = publicKey
			cfg.Serve.WebPush.VAPIDPrivateKey = privateKey
			s := &serveServer{store: store, cfgRef: cfg}
			if tt.oversized {
				s.cfg.uiTitle = strings.Repeat("x", appnotify.WebPushRecordSize)
			}
			sub, err := store.UpsertPushSubscription(context.Background(), &session.PushSubscription{
				Endpoint:   endpoint.URL,
				KeyP256DH:  base64.RawURLEncoding.EncodeToString(browserKey.PublicKey().Bytes()),
				KeyAuth:    base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
				VAPIDKeyID: appnotify.WebPushKeyID(publicKey),
			})
			if err != nil {
				t.Fatal(err)
			}
			s.enqueueCompletionPush("resp-1", "sess-1", sub.ID, "completed", time.Now())
			s.dispatchCompletionPushes(store)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var status, lastError string
			var attempts int
			if err := db.QueryRow(`SELECT status, attempt_count, last_error FROM completion_push_outbox WHERE response_id = 'resp-1'`).Scan(&status, &attempts, &lastError); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantRequests := "delivered", int32(1)
			if tt.rejectAll {
				wantStatus = "dead"
				if lastError != "push payload too large" {
					t.Errorf("last_error = %q, want push payload too large", lastError)
				}
			}
			if tt.httpStatus == http.StatusRequestEntityTooLarge {
				wantRequests = 2
			}
			if status != wantStatus || attempts != 1 || requests.Load() != wantRequests {
				t.Fatalf("status=%s attempts=%d requests=%d; want %s/1/%d", status, attempts, requests.Load(), wantStatus, wantRequests)
			}
			due, err := store.ListDueCompletionPushes(context.Background(), time.Now().Add(time.Hour), 25)
			if err != nil {
				t.Fatal(err)
			}
			if len(due) != 0 {
				t.Errorf("push remains eligible for retry: %v", due)
			}
			active, err := store.GetPushSubscription(context.Background(), sub.ID)
			if err != nil {
				t.Fatal(err)
			}
			if active == nil || active.Status != "active" {
				t.Errorf("payload size failure invalidated subscription: %+v", active)
			}
		})
	}
}
