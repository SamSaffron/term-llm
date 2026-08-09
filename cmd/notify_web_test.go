package cmd

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestNormalizeWebPushSubject(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty uses https default", input: "", want: "https://github.com/samsaffron/term-llm"},
		{name: "whitespace empty uses https default", input: "   ", want: "https://github.com/samsaffron/term-llm"},
		{name: "bare email kept", input: "test@example.com", want: "test@example.com"},
		{name: "mailto stripped once", input: "mailto:test@example.com", want: "test@example.com"},
		{name: "mailto stripped case insensitive", input: "MAILTO:test@example.com", want: "test@example.com"},
		{name: "https URL kept", input: "https://example.com", want: "https://example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeWebPushSubject(tt.input); got != tt.want {
				t.Fatalf("normalizeWebPushSubject(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSendWebPushAllRemovesStaleSubscriptions(t *testing.T) {
	oldNoSession := noSession
	oldSessionDB := sessionDBPath
	t.Cleanup(func() {
		noSession = oldNoSession
		sessionDBPath = oldSessionDB
	})
	noSession = false
	sessionDBPath = ""

	browserKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate browser key: %v", err)
	}
	p256dh := base64.RawURLEncoding.EncodeToString(browserKey.PublicKey().Bytes())
	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}
	auth := base64.RawURLEncoding.EncodeToString(authSecret)

	vapidPrivate, vapidPublic, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("generate VAPID keys: %v", err)
	}

	tests := []struct {
		name          string
		status        int
		wantRemaining int
		wantErrors    bool
		wantRequests  int32
	}{
		{name: "gone", status: http.StatusGone, wantRemaining: 0, wantRequests: 1},
		{name: "not found", status: http.StatusNotFound, wantRemaining: 0, wantRequests: 1},
		{name: "server error", status: http.StatusInternalServerError, wantRemaining: 1, wantErrors: true, wantRequests: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(tt.status)
			}))
			defer pushServer.Close()

			dbPath := filepath.Join(t.TempDir(), "sessions.db")
			store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			if err := store.SavePushSubscription(context.Background(), &session.PushSubscription{
				Endpoint:  pushServer.URL,
				KeyP256DH: p256dh,
				KeyAuth:   auth,
			}); err != nil {
				t.Fatalf("SavePushSubscription: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("close store: %v", err)
			}

			cfg := &config.Config{
				Sessions: config.SessionsConfig{Enabled: true, Path: dbPath},
				Serve: config.ServeConfig{WebPush: config.WebPushConfig{
					VAPIDPublicKey:  vapidPublic,
					VAPIDPrivateKey: vapidPrivate,
				}},
			}

			for attempt := 1; attempt <= 2; attempt++ {
				sent, errs := sendWebPushAll(context.Background(), cfg, "test", io.Discard)
				if sent != 0 {
					t.Fatalf("attempt %d sent = %d, want 0", attempt, sent)
				}
				if gotErrors := len(errs) > 0; gotErrors != tt.wantErrors {
					t.Fatalf("attempt %d errors = %v, want errors %t", attempt, errs, tt.wantErrors)
				}
			}

			store, err = session.NewStore(session.Config{Enabled: true, Path: dbPath})
			if err != nil {
				t.Fatalf("reopen store: %v", err)
			}
			defer store.Close()
			subs, err := store.ListPushSubscriptions(context.Background())
			if err != nil {
				t.Fatalf("ListPushSubscriptions: %v", err)
			}
			if len(subs) != tt.wantRemaining {
				t.Fatalf("remaining subscriptions = %d, want %d", len(subs), tt.wantRemaining)
			}
			if got := requests.Load(); got != tt.wantRequests {
				t.Fatalf("push requests = %d, want %d", got, tt.wantRequests)
			}
		})
	}
}
