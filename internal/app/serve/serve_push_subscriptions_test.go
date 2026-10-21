package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestPushSubscribeClientKind(t *testing.T) {
	for _, tt := range []struct {
		name       string
		kind       string
		wantKind   string
		wantStatus int
	}{
		{name: "legacy defaults to browser", wantKind: session.PushClientBrowser, wantStatus: http.StatusCreated},
		{name: "browser", kind: "browser", wantKind: session.PushClientBrowser, wantStatus: http.StatusCreated},
		{name: "app", kind: "app", wantKind: session.PushClientApp, wantStatus: http.StatusCreated},
		{name: "unknown", kind: "telegram", wantStatus: http.StatusBadRequest},
		{name: "case sensitive", kind: "APP", wantStatus: http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			srv := &serveServer{store: store}
			body, err := json.Marshal(map[string]any{
				"endpoint": "https://push.example/sub", "keys": map[string]string{"p256dh": "key", "auth": "auth"},
				"client_kind": tt.kind,
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.handlePushSubscribe(rr, req)
			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tt.wantStatus, rr.Body.String())
			}
			subs, err := store.ListPushSubscriptions(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantStatus == http.StatusBadRequest {
				if len(subs) != 0 {
					t.Fatalf("invalid client kind persisted: %+v", subs)
				}
				return
			}
			if len(subs) != 1 || subs[0].ClientKind != tt.wantKind {
				t.Fatalf("subscriptions = %+v, want one %s subscription", subs, tt.wantKind)
			}
		})
	}
}

func TestPushSubscribeAppReenrollment(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	legacy, err := store.UpsertPushSubscription(ctx, &session.PushSubscription{
		Endpoint: "https://relay.example/sub", KeyP256DH: "key", KeyAuth: "auth",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(`{"endpoint":"https://relay.example/sub","keys":{"p256dh":"key","auth":"auth"},"client_kind":"app"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handlePushSubscribe(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	updated, err := store.GetPushSubscription(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated == nil || updated.ClientKind != session.PushClientApp {
		t.Fatalf("reenrollment did not reclassify existing subscription: %+v", updated)
	}
	subs, err := store.ListPushSubscriptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("reenrollment duplicated subscription: %+v", subs)
	}
}
