package cmd

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestUICacheScopeFor(t *testing.T) {
	const storeID = "private-store-instance-id"
	baseline := uiCacheScopeFor(storeID, "none")
	if len(baseline) != 24 {
		t.Fatalf("cache scope length = %d, want 24", len(baseline))
	}
	if _, err := hex.DecodeString(baseline); err != nil {
		t.Fatalf("cache scope is not hex: %v", err)
	}
	if strings.Contains(baseline, storeID) {
		t.Fatal("cache scope exposes the store instance ID")
	}
	if got := uiCacheScopeFor(storeID, "none"); got != baseline {
		t.Fatalf("cache scope is not deterministic: %q != %q", got, baseline)
	}
	seen := map[string]bool{baseline: true}
	for _, tc := range []struct {
		storeID string
		mode    string
	}{
		{storeID, "bearer"},
		{storeID, "passkey"},
		{"other-store-instance-id", "none"},
	} {
		got := uiCacheScopeFor(tc.storeID, tc.mode)
		if seen[got] {
			t.Fatalf("scope did not isolate store %q mode %q: %q", tc.storeID, tc.mode, got)
		}
		seen[got] = true
	}
}

func TestBuildIndexHTMLCacheScopeShellAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name        string
		requireAuth bool
		want        string
	}{
		{"no auth", false, `window.TERM_LLM_SHELL_AUTHORIZED=true;`},
		{"bearer", true, `window.TERM_LLM_SHELL_AUTHORIZED=false;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &serveServer{cfg: serveServerConfig{basePath: "/ui", requireAuth: tc.requireAuth}}
			body := string(srv.buildIndexHTML(""))
			if !strings.Contains(body, tc.want) {
				t.Fatalf("index missing %q", tc.want)
			}
			if strings.Contains(body, "TERM_LLM_CACHE_SCOPE=") {
				t.Fatal("a server without a persistent store must not inject a cache scope")
			}
		})
	}
}

func TestBuildIndexHTMLCacheScopePersistentStore(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	storeID, err := store.StoreInstanceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"none", "bearer", "passkey"} {
		t.Run(mode, func(t *testing.T) {
			srv := &serveServer{store: store, cfg: serveServerConfig{basePath: "/ui", requireAuth: mode != "none"}}
			if mode == "passkey" {
				srv.browserAuth = &browserPasskeyHandler{}
			}
			body := string(srv.buildIndexHTML(""))
			want := `window.TERM_LLM_CACHE_SCOPE="` + uiCacheScopeFor(storeID, mode) + `";`
			if !strings.Contains(body, want) {
				t.Fatalf("index missing %q", want)
			}
			if strings.Contains(body, storeID) {
				t.Fatal("index exposes the store instance ID")
			}
			if mode == "passkey" && !strings.Contains(body, `window.TERM_LLM_SHELL_AUTHORIZED=true;`) {
				t.Fatal("passkey-gated shell should authorize immediate cached display")
			}
		})
	}
}
