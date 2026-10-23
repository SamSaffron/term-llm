package runtime

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestInitSessionStoreWithFutureSchemaDoesNotOfferReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A deliberately far-future marker avoids coupling this test to the current
	// session schema version. Sentinel data must survive failed initialization.
	const futureVersion = 2147483647
	if _, err := db.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (2147483647);
		CREATE TABLE sentinel (value TEXT);
		INSERT INTO sentinel VALUES ('keep me');
	`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Sessions: config.SessionsConfig{Enabled: true, Path: path}}
	var output bytes.Buffer
	store, cleanup := InitSessionStoreWith(cfg, &output, SessionStoreOverrides{})
	cleanup()
	if store != nil {
		t.Fatal("future schema opened a store")
	}
	message := output.String()
	for _, want := range []string{"newer than supported version", "Upgrade this term-llm binary", "Session persistence is disabled"} {
		if !strings.Contains(message, want) {
			t.Errorf("output lacks %q: %s", want, message)
		}
	}
	for _, forbidden := range []string{"reset", "delete", "[y/n]", "schema errors"} {
		if strings.Contains(strings.ToLower(message), forbidden) {
			t.Errorf("future schema output contains %q: %s", forbidden, message)
		}
	}

	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != futureVersion {
		t.Fatalf("schema marker changed: version=%d err=%v", version, err)
	}
	var sentinel string
	if err := db.QueryRow(`SELECT value FROM sentinel`).Scan(&sentinel); err != nil || sentinel != "keep me" {
		t.Fatalf("database data changed: value=%q err=%v", sentinel, err)
	}
}

func TestSessionStoreSchemaErrorClassification(t *testing.T) {
	future := errors.New("session database schema version 67 is newer than supported version 65")
	for _, tc := range []struct {
		name   string
		err    error
		future bool
		reset  bool
	}{
		{"nil", nil, false, false},
		{"future", future, true, false},
		{"wrapped future", fmt.Errorf("initialize schema: %w", future), true, false},
		{"missing column", errors.New("no such column: example"), false, true},
		{"missing table", errors.New("no such table: sessions"), false, true},
		{"logic error", errors.New("SQL logic error"), false, true},
		{"malformed", errors.New("database disk image is malformed"), false, true},
		{"not database", errors.New("file is not a database"), false, true},
		{"base schema", errors.New("create base schema: failed"), false, true},
		{"initialization", errors.New("initialize schema: failed"), false, true},
		{"unrelated", errors.New("permission denied"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNewerSessionSchemaError(tc.err); got != tc.future {
				t.Errorf("future schema = %t, want %t", got, tc.future)
			}
			if got := isSchemaError(tc.err); got != tc.reset {
				t.Errorf("resettable schema error = %t, want %t", got, tc.reset)
			}
		})
	}
}
