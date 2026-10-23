package session

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPushSubscriptionLifecyclePreservesCanonicalID(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first, err := store.UpsertPushSubscription(ctx, &PushSubscription{
		Endpoint: "https://push.example/one", KeyP256DH: "key-one", KeyAuth: "auth-one", VAPIDKeyID: "vapid-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.UpsertPushSubscription(ctx, &PushSubscription{
		Endpoint: "https://push.example/one", KeyP256DH: "key-two", KeyAuth: "auth-two", VAPIDKeyID: "vapid-two",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || second.ID != first.ID {
		t.Fatalf("canonical ids first=%q second=%q", first.ID, second.ID)
	}
	if second.KeyP256DH != "key-two" || second.VAPIDKeyID != "vapid-two" || second.Status != "active" {
		t.Fatalf("updated subscription = %#v", second)
	}
	if err := store.MarkPushSubscriptionStale(ctx, first.ID, "http_410", "expired endpoint"); err != nil {
		t.Fatal(err)
	}
	stale, err := store.GetPushSubscription(ctx, first.ID)
	if err != nil || stale == nil || stale.Status != "stale" || stale.LastFailureCode != "http_410" {
		t.Fatalf("stale subscription = %#v err=%v", stale, err)
	}
}
