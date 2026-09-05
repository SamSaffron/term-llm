package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
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
			s := &serveServer{store: store, cfgRef: cfg}
			for _, name := range []string{"slow", "healthy"} {
				sub, err := store.UpsertPushSubscription(context.Background(), &session.PushSubscription{
					Endpoint: "https://push.example/" + name, KeyP256DH: "key", KeyAuth: "auth",
					VAPIDKeyID: webPushKeyID(cfg.Serve.WebPush.VAPIDPublicKey),
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
