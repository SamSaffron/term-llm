package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestInitSessionStore_NoSessionFlagBypassesStore(t *testing.T) {
	oldNoSession := noSession
	oldSessionDB := sessionDBPath
	t.Cleanup(func() {
		noSession = oldNoSession
		sessionDBPath = oldSessionDB
	})

	noSession = true
	sessionDBPath = ""

	cfg := &config.Config{
		Sessions: config.SessionsConfig{
			Enabled: true,
		},
	}

	store, cleanup := InitSessionStore(cfg, io.Discard)
	defer cleanup()

	if store != nil {
		t.Fatal("expected nil store when --no-session is enabled")
	}
}

func TestInitSessionStore_UsesSessionDBPathOverride(t *testing.T) {
	oldNoSession := noSession
	oldSessionDB := sessionDBPath
	t.Cleanup(func() {
		noSession = oldNoSession
		sessionDBPath = oldSessionDB
	})

	noSession = false
	dbPath := filepath.Join(t.TempDir(), "custom", "sessions.db")
	sessionDBPath = dbPath

	cfg := &config.Config{
		Sessions: config.SessionsConfig{
			Enabled: true,
		},
	}

	store, cleanup := InitSessionStore(cfg, io.Discard)
	defer cleanup()

	if store == nil {
		t.Fatal("expected session store to be initialized")
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected session database at override path %q: %v", dbPath, err)
	}
}

func TestInitSessionStore_UsesConfigSessionPath(t *testing.T) {
	oldNoSession := noSession
	oldSessionDB := sessionDBPath
	t.Cleanup(func() {
		noSession = oldNoSession
		sessionDBPath = oldSessionDB
	})

	noSession = false
	sessionDBPath = ""
	dbPath := filepath.Join(t.TempDir(), "from-config", "sessions.db")

	cfg := &config.Config{
		Sessions: config.SessionsConfig{
			Enabled: true,
			Path:    dbPath,
		},
	}

	store, cleanup := InitSessionStore(cfg, io.Discard)
	defer cleanup()

	if store == nil {
		t.Fatal("expected session store to be initialized")
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected session database at config path %q: %v", dbPath, err)
	}
}
