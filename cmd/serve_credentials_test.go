package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestCompletionPushCredentialFailureDoesNotMarkStale(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sub, err := store.UpsertPushSubscription(ctx, &session.PushSubscription{
		Endpoint: "https://push.example.test/one", KeyP256DH: "key", KeyAuth: "auth", VAPIDKeyID: webPushKeyID("public-key"),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "public-key")
	cfg := &config.Config{}
	cfg.Serve.WebPush.VAPIDPublicKey = "file://" + path
	srv := &serveServer{store: store, cfgRef: cfg}
	if _, err := srv.validateCompletionPushTarget(ctx, sub.ID); err == nil {
		t.Fatal("expected missing key error")
	}
	rec := httptest.NewRecorder()
	srv.handlePushSubscribe(rec, httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("subscribe with unavailable key = %d", rec.Code)
	}
	stored, err := store.GetPushSubscription(ctx, sub.ID)
	if err != nil || stored.Status != "active" {
		t.Fatalf("resolution failure mutated subscription: %#v, %v", stored, err)
	}
	if err := os.WriteFile(path, []byte("public-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, err := srv.validateCompletionPushTarget(ctx, sub.ID); err != nil || id != sub.ID {
		t.Fatalf("retry = %q, %v", id, err)
	}
	cfg.Serve.WebPush.VAPIDPublicKey = "rotated-key"
	if _, err := srv.validateCompletionPushTarget(ctx, sub.ID); err == nil {
		t.Fatal("expected rotation error")
	}
	stored, err = store.GetPushSubscription(ctx, sub.ID)
	if err != nil || stored.Status != "stale" || stored.LastFailureCode != "vapid_rotated" {
		t.Fatalf("real rotation did not mark stale: %#v, %v", stored, err)
	}
}

func TestRenderIndexHTMLRetriesCredentialFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public-key")
	cfg := &config.Config{}
	cfg.Serve.WebPush.VAPIDPublicKey = "file://" + path
	srv := &serveServer{cfgRef: cfg}
	if html := string(srv.renderIndexHTML()); strings.Contains(html, "TERM_LLM_VAPID_PUBLIC_KEY") {
		t.Fatal("failed resolution injected a VAPID key")
	}
	if err := os.WriteFile(path, []byte("public-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	html := srv.renderIndexHTML()
	if !strings.Contains(string(html), `TERM_LLM_VAPID_PUBLIC_KEY="public-key"`) {
		t.Fatal("successful retry did not inject VAPID key")
	}
	cached := srv.renderIndexHTML()
	if &html[0] != &cached[0] {
		t.Fatal("successful HTML was not cached")
	}
}

func TestCompletionPushEmptyKeysLeaveOutboxPending(t *testing.T) {
	for _, emptyKey := range []string{"public", "private"} {
		t.Run(emptyKey, func(t *testing.T) {
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			sub, err := store.UpsertPushSubscription(ctx, &session.PushSubscription{
				Endpoint: "https://push.example.test/one", KeyP256DH: "key", KeyAuth: "auth", VAPIDKeyID: webPushKeyID("public-key"),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.EnqueueCompletionPush(ctx, session.CompletionPushOutboxItem{
				EventID: "completion", ResponseID: "response", SubscriptionID: sub.ID, Payload: []byte(`{}`),
			}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "empty")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{}
			cfg.Serve.WebPush.VAPIDPublicKey = "public-key"
			cfg.Serve.WebPush.VAPIDPrivateKey = "private-key"
			if emptyKey == "public" {
				cfg.Serve.WebPush.VAPIDPublicKey = "file://" + path
			} else {
				cfg.Serve.WebPush.VAPIDPrivateKey = "file://" + path
			}
			srv := &serveServer{store: store, cfgRef: cfg}
			srv.dispatchCompletionPushes(store)
			pending, err := store.ListDueCompletionPushes(ctx, time.Now().Add(time.Hour), 10)
			if err != nil || len(pending) != 1 || pending[0].AttemptCount != 0 {
				t.Fatalf("empty %s key consumed outbox work: %#v, %v", emptyKey, pending, err)
			}
		})
	}
}
