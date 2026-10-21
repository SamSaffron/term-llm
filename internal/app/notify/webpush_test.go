package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
			if got := NormalizeWebPushSubject(tt.input); got != tt.want {
				t.Fatalf("NormalizeWebPushSubject(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSendWebPushAllRemovesStaleSubscriptions(t *testing.T) {
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
		wantStale     bool
		oversized     bool
	}{
		{name: "oversized fails without sending", oversized: true, wantRemaining: 1, wantErrors: true},
		{name: "gone", status: http.StatusGone, wantRemaining: 1, wantRequests: 1, wantStale: true},
		{name: "not found", status: http.StatusNotFound, wantRemaining: 1, wantRequests: 1, wantStale: true},
		{name: "server error", status: http.StatusInternalServerError, wantRemaining: 1, wantErrors: true, wantRequests: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read push body: %v", err)
				}
				if len(body) != 2048 {
					t.Errorf("encrypted body length = %d, want 2048", len(body))
				}
				if len(body) < 20 || binary.BigEndian.Uint32(body[16:20]) != 2048 {
					t.Errorf("encrypted record header does not specify size 2048")
				}
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
				message := "test"
				if tt.oversized {
					message = strings.Repeat("x", 2048)
				}
				sent, errs := SendWebPushAll(context.Background(), testDeps(), cfg, message, io.Discard)
				if tt.oversized && (len(errs) != 1 || !strings.Contains(errs[0], webpush.ErrMaxPadExceeded.Error())) {
					t.Fatalf("oversized payload errors = %v, want maximum length error", errs)
				}
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
			if len(subs) > 0 && (subs[0].Status == "stale") != tt.wantStale {
				t.Fatalf("subscription status = %q, want stale %t", subs[0].Status, tt.wantStale)
			}
			if got := requests.Load(); got != tt.wantRequests {
				t.Fatalf("push requests = %d, want %d", got, tt.wantRequests)
			}
		})
	}
}

func testDeps() Deps {
	return Deps{InitSessionStore: func(cfg *config.Config, _ io.Writer) (session.Store, func()) {
		store, err := session.NewStore(session.Config{Enabled: cfg.Sessions.Enabled, Path: cfg.Sessions.Path})
		if err != nil {
			return nil, func() {}
		}
		return store, func() { _ = store.Close() }
	}}
}

func TestNormalizeTelegramParseMode(t *testing.T) {
	for in, want := range map[string]string{"": "Markdown", " html ": "HTML", "MARKDOWN": "Markdown"} {
		got, err := NormalizeTelegramParseMode(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeTelegramParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeTelegramParseMode("xml"); err == nil {
		t.Fatal("expected error for invalid parse mode")
	}
}

func TestRunWebRequiresVAPIDKeys(t *testing.T) {
	deps := Deps{LoadConfig: func() (*config.Config, error) { return &config.Config{}, nil }}
	err := RunWeb(context.Background(), deps, WebOptions{Message: "x", Stderr: io.Discard})
	if err == nil {
		t.Fatal("expected error without VAPID keys")
	}
}

func TestRunBroadcastNoPlatforms(t *testing.T) {
	deps := Deps{LoadConfig: func() (*config.Config, error) { return &config.Config{}, nil }}
	err := RunBroadcast(context.Background(), deps, BroadcastOptions{Message: "x", Stderr: io.Discard})
	if err == nil {
		t.Fatal("expected no-platforms error")
	}
}
