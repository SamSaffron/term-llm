package runtime

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSessionStoreConfigWithExplicitOverrides(t *testing.T) {
	cfg := &config.Config{Sessions: config.SessionsConfig{Enabled: true, Path: " configured.db ", MaxAgeDays: 7, MaxCount: 12, StripImageBase64: true}}
	before := cfg.Sessions
	for _, tc := range []struct {
		name      string
		overrides SessionStoreOverrides
		enabled   bool
		path      string
	}{
		{"configured", SessionStoreOverrides{}, true, "configured.db"},
		{"override", SessionStoreOverrides{DBPath: " explicit.db "}, true, "explicit.db"},
		{"empty override", SessionStoreOverrides{DBPath: " \t "}, true, "configured.db"},
		{"no session and explicit path", SessionStoreOverrides{NoSession: true, DBPath: "explicit.db"}, false, "explicit.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := session.Config{Enabled: tc.enabled, Path: tc.path, MaxAgeDays: 7, MaxCount: 12, StripImageBase64: true}
			if got := SessionStoreConfigWith(cfg, tc.overrides); !reflect.DeepEqual(got, want) {
				t.Fatalf("config = %#v, want %#v", got, want)
			}
			if !reflect.DeepEqual(cfg.Sessions, before) {
				t.Fatal("resolution mutated config")
			}
		})
	}
	cfg.Sessions.Enabled = false
	if SessionStoreConfigWith(cfg, SessionStoreOverrides{}).Enabled {
		t.Fatal("override enabled disabled configuration")
	}
}

func TestInitSessionStoreWithUsesExplicitPathAndDisable(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "configured.db")
	selected := filepath.Join(t.TempDir(), "explicit", "sessions.db")
	cfg := &config.Config{Sessions: config.SessionsConfig{Enabled: true, Path: configured}}
	store, cleanup := InitSessionStoreWith(cfg, io.Discard, SessionStoreOverrides{NoSession: true, DBPath: selected})
	cleanup()
	if store != nil {
		t.Fatal("NoSession opened a store")
	}
	if _, err := os.Stat(selected); !os.IsNotExist(err) {
		t.Fatalf("disabled store touched disk: %v", err)
	}
	store, cleanup = InitSessionStoreWith(cfg, io.Discard, SessionStoreOverrides{DBPath: selected})
	defer cleanup()
	if store == nil {
		t.Fatal("explicit store unavailable")
	}
	if _, err := os.Stat(selected); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configured); !os.IsNotExist(err) {
		t.Fatalf("configured path used instead of override: %v", err)
	}
}
