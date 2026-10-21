package session

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPushSubscriptionClientKindDefaultsAndUpserts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	var canonicalID string
	for _, kind := range []string{"", PushClientApp, PushClientBrowser, PushClientApp, ""} {
		sub, err := store.UpsertPushSubscription(ctx, &PushSubscription{
			Endpoint: "https://push.example/client", KeyP256DH: "key", KeyAuth: "auth", ClientKind: kind,
		})
		if err != nil {
			t.Fatal(err)
		}
		want := kind
		if want == "" {
			want = PushClientBrowser
		}
		if canonicalID == "" {
			canonicalID = sub.ID
		}
		if sub.ID != canonicalID || sub.ClientKind != want {
			t.Fatalf("upsert %q = %+v, want ID %q and kind %q", kind, sub, canonicalID, want)
		}
		got, err := store.GetPushSubscription(ctx, canonicalID)
		if err != nil || got == nil || got.ClientKind != want {
			t.Fatalf("get = %+v, %v; want kind %q", got, err, want)
		}
		subs, err := store.ListPushSubscriptions(ctx)
		if err != nil || len(subs) != 1 || subs[0].ClientKind != want {
			t.Fatalf("list = %+v, %v; want kind %q", subs, err, want)
		}
	}
	legacy := &PushSubscription{Endpoint: "https://push.example/legacy", KeyP256DH: "key", KeyAuth: "auth"}
	if err := store.SavePushSubscription(ctx, legacy); err != nil || legacy.ClientKind != PushClientBrowser {
		t.Fatalf("legacy save = %+v, %v", legacy, err)
	}
	app := &PushSubscription{Endpoint: "https://push.example/app", KeyP256DH: "key", KeyAuth: "auth", ClientKind: PushClientApp}
	if err := store.SavePushSubscription(ctx, app); err != nil {
		t.Fatal(err)
	}
	// Reopening read-only must detect the new column rather than use the legacy fallback.
	reader, err := NewSQLiteStore(Config{Enabled: true, Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := reader.GetPushSubscription(ctx, app.ID)
	if err != nil || got == nil || got.ClientKind != PushClientApp {
		t.Fatalf("read-only persisted app = %+v, %v", got, err)
	}
}

func TestPushSubscriptionClientKindRejectsInvalidUpserts(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	existing := &PushSubscription{Endpoint: "https://push.example/existing", KeyP256DH: "key", KeyAuth: "auth", ClientKind: PushClientApp}
	if err := store.SavePushSubscription(ctx, existing); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"APP", "native", " app ", " "} {
		t.Run(kind, func(t *testing.T) {
			for _, endpoint := range []string{existing.Endpoint, "https://push.example/new"} {
				_, err := store.UpsertPushSubscription(ctx, &PushSubscription{
					Endpoint: endpoint, KeyP256DH: "changed", KeyAuth: "changed", ClientKind: kind,
				})
				if err == nil || !strings.Contains(err.Error(), "invalid client kind") {
					t.Fatalf("upsert %q error = %v", kind, err)
				}
			}
		})
	}
	subs, err := store.ListPushSubscriptions(ctx)
	if err != nil || len(subs) != 1 || subs[0].ClientKind != PushClientApp || subs[0].KeyP256DH != "key" {
		t.Fatalf("invalid upserts changed stored rows: %+v, %v", subs, err)
	}
}
